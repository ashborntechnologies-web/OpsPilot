package github

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const githubAPIBase = "https://api.github.com"

type Service struct {
	clientID      string
	clientSecret  string
	redirectURL   string
	encryptionKey string
	// prevKey is the previous ENCRYPTION_KEY used during rotation.
	// Set via ENCRYPTION_KEY_PREV. Old tokens are decrypted with this key;
	// new encryptions always use encryptionKey.
	prevKey string
	// anthropicKey enables the AI framework-detection fallback. Empty = AI path
	// never runs (rule-based detection only).
	anthropicKey string
	llm          *llm.Client
	db           *models.DB
}

type Repo struct {
	ID    int    `json:"id"`
	FullName string `json:"full_name"`
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Private     bool   `json:"private"`
	HTMLURL     string `json:"html_url"`
	Description string `json:"description"`
	Language    string `json:"language"`
}

type DetectResult struct {
	Framework  string   `json:"framework"`
	Confidence string   `json:"confidence"` // high | medium | low
	Signals    []string `json:"signals"`
	HasDocker  bool     `json:"has_dockerfile"`

	// AI-inferred fields — populated only when the AI fallback runs (AIDetected=true).
	StartCommand string   `json:"start_command,omitempty"` // AI-inferred production start command
	BuildCommand string   `json:"build_command,omitempty"` // AI-inferred build command (SPA/static)
	Warnings     []string `json:"warnings,omitempty"`      // AI-noticed deployment concerns
	AIDetected   bool     `json:"ai_detected"`             // true if the AI fallback produced this result
}

func NewService(clientID, clientSecret, redirectURL, encryptionKey string, db *models.DB, anthropicKey string) *Service {
	return &Service{
		clientID:      clientID,
		clientSecret:  clientSecret,
		redirectURL:   redirectURL,
		encryptionKey: encryptionKey,
		prevKey:       os.Getenv("ENCRYPTION_KEY_PREV"),
		anthropicKey:  anthropicKey,
		llm:           llm.New(anthropicKey),
		db:            db,
	}
}

// HandleGetOAuthURL is a protected endpoint — returns the GitHub OAuth URL for the
// frontend to redirect to. Uses a signed state so the callback can identify the user
// without needing a session.
func (s *Service) HandleGetOAuthURL(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	state, err := generateState(userID, s.encryptionKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state"})
		return
	}

	oauthURL := fmt.Sprintf(
		"https://github.com/login/oauth/authorize?client_id=%s&redirect_uri=%s&scope=repo&state=%s",
		s.clientID,
		url.QueryEscape(s.redirectURL),
		url.QueryEscape(state),
	)

	c.JSON(http.StatusOK, gin.H{"url": oauthURL})
}

// HandleOAuthCallback is public — GitHub redirects here after the user authorizes.
// It validates the signed state, exchanges the code, encrypts the token, and stores it.
func (s *Service) HandleOAuthCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")

	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing code"})
		return
	}

	userID, err := verifyState(state, s.encryptionKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired state"})
		return
	}

	token, err := s.exchangeCode(code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to exchange code"})
		return
	}

	encrypted, err := encryptToken(token, s.encryptionKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt token"})
		return
	}

	_, err = s.db.Pool.Exec(c.Request.Context(),
		`UPDATE users SET github_token = $1, updated_at = NOW() WHERE id = $2`,
		encrypted, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store token"})
		return
	}

	// Redirect the browser back to the frontend
	frontendURL := os.Getenv("FRONTEND_URL")
	c.Redirect(http.StatusTemporaryRedirect, frontendURL+"?github=connected")
}

// HandleListRepos returns repos accessible to the authenticated user.
func (s *Service) HandleListRepos(c *gin.Context) {
	token, err := s.getTokenForUser(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "GitHub not connected"})
		return
	}

	repos, err := s.fetchRepos(c.Request.Context(), token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch repos"})
		return
	}

	c.JSON(http.StatusOK, repos)
}

// HandleDetectFramework analyzes a repo and returns the detected framework.
func (s *Service) HandleDetectFramework(c *gin.Context) {
	owner := c.Param("owner")
	repo := c.Param("repo")

	token, err := s.getTokenForUser(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "GitHub not connected"})
		return
	}

	result, err := s.DetectFramework(c.Request.Context(), token, owner, repo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to detect framework"})
		return
	}

	c.JSON(http.StatusOK, result)
}

// DetectFramework analyzes repo contents to identify the framework.
func (s *Service) DetectFramework(ctx context.Context, token, owner, repo string) (*DetectResult, error) {
	files, err := s.listRootFiles(ctx, token, owner, repo)
	if err != nil {
		return nil, err
	}

	result := &DetectResult{}
	fileSet := make(map[string]bool)
	for _, f := range files {
		fileSet[strings.ToLower(f)] = true
	}

	result.HasDocker = fileSet["dockerfile"]

	switch {
	case fileSet["pom.xml"] || fileSet["build.gradle"] || fileSet["build.gradle.kts"]:
		result.Framework = models.FrameworkSpring
		result.Confidence = "high"
		result.Signals = append(result.Signals, "Maven/Gradle build file detected (Spring Boot)")

	case fileSet["gemfile"]:
		result.Framework = models.FrameworkRails
		result.Confidence = "high"
		result.Signals = append(result.Signals, "Gemfile detected (Ruby on Rails)")

	case fileSet["go.mod"]:
		result.Framework = models.FrameworkGo
		result.Confidence = "high"
		result.Signals = append(result.Signals, "go.mod detected")

	case fileSet["next.config.js"] || fileSet["next.config.ts"] || fileSet["next.config.mjs"]:
		result.Framework = models.FrameworkNextJS
		result.Confidence = "high"
		result.Signals = append(result.Signals, "next.config.* detected")

	case fileSet["manage.py"]:
		result.Framework = models.FrameworkDjango
		result.Confidence = "high"
		result.Signals = append(result.Signals, "manage.py detected (Django)")

	case fileSet["requirements.txt"] || fileSet["pyproject.toml"]:
		if fileSet["main.py"] {
			content, _ := s.fetchFileContentDecoded(ctx, token, owner, repo, "main.py")
			switch {
			case strings.Contains(content, "fastapi") || strings.Contains(content, "FastAPI"):
				result.Framework = models.FrameworkFastAPI
				result.Confidence = "high"
				result.Signals = append(result.Signals, "FastAPI import in main.py")
			case strings.Contains(content, "flask") || strings.Contains(content, "Flask"):
				result.Framework = models.FrameworkFlask
				result.Confidence = "high"
				result.Signals = append(result.Signals, "Flask import in main.py")
			default:
				result.Framework = models.FrameworkPython
				result.Confidence = "medium"
				result.Signals = append(result.Signals, "Python requirements found, no specific framework detected")
			}
		} else {
			result.Framework = models.FrameworkPython
			result.Confidence = "medium"
			result.Signals = append(result.Signals, "requirements.txt found, no main.py framework hints")
		}

	case fileSet["package.json"]:
		// Read package.json to detect specific JS/TS frameworks by their deps
		pkg, _ := s.fetchFileContentDecoded(ctx, token, owner, repo, "package.json")
		switch {
		case strings.Contains(pkg, `"react-scripts"`):
			result.Framework = models.FrameworkReactSPA
			result.Confidence = "high"
			result.Signals = append(result.Signals, "react-scripts in package.json (Create React App)")
		case strings.Contains(pkg, `"vite"`) &&
			!strings.Contains(pkg, `"express"`) &&
			!strings.Contains(pkg, `"fastify"`):
			result.Framework = models.FrameworkVite
			result.Confidence = "high"
			result.Signals = append(result.Signals, "vite in package.json (Vite SPA)")
		case strings.Contains(pkg, `"@nestjs/core"`):
			result.Framework = models.FrameworkNestJS
			result.Confidence = "high"
			result.Signals = append(result.Signals, "@nestjs/core in package.json")
		case strings.Contains(pkg, `"@remix-run/node"`) || strings.Contains(pkg, `"@remix-run/react"`):
			result.Framework = models.FrameworkRemix
			result.Confidence = "high"
			result.Signals = append(result.Signals, "@remix-run in package.json")
		case strings.Contains(pkg, `"nuxt"`):
			result.Framework = models.FrameworkNuxtJS
			result.Confidence = "high"
			result.Signals = append(result.Signals, "nuxt in package.json")
		case strings.Contains(pkg, `"@sveltejs/kit"`):
			result.Framework = models.FrameworkSvelteKit
			result.Confidence = "high"
			result.Signals = append(result.Signals, "@sveltejs/kit in package.json")
		case strings.Contains(pkg, `"astro"`):
			result.Framework = models.FrameworkAstro
			result.Confidence = "high"
			result.Signals = append(result.Signals, "astro in package.json")
		case strings.Contains(pkg, `"express"`):
			result.Framework = models.FrameworkExpress
			result.Confidence = "high"
			result.Signals = append(result.Signals, "express in package.json")
		default:
			result.Framework = models.FrameworkNodeJS
			result.Confidence = "medium"
			result.Signals = append(result.Signals, "package.json detected, no specific framework identified")
		}

	default:
		switch {
		case fileSet["index.html"]:
			result.Framework = models.FrameworkStatic
			result.Confidence = "medium"
			result.Signals = append(result.Signals, "index.html found, no backend detected")
		default:
			hasPy := false
			for _, f := range files {
				if strings.HasSuffix(strings.ToLower(f), ".py") {
					hasPy = true
					break
				}
			}
			if hasPy {
				result.Framework = models.FrameworkPython
				result.Confidence = "medium"
				result.Signals = append(result.Signals, "Python files detected (no requirements.txt)")
			} else {
				result.Framework = ""
				result.Confidence = "low"
				result.Signals = append(result.Signals, "no recognized framework files found")
			}
		}
	}

	// AI fallback: kick in when rule-based detection is not confident.
	if result.Confidence != "high" && s.anthropicKey != "" {
		if aiResult, err := s.aiDetect(ctx, token, owner, repo, files, result); err == nil {
			return aiResult, nil
		}
	}

	return result, nil
}

// aiDetectSystemPrompt instructs Claude to return a strict JSON deployment profile.
const aiDetectSystemPrompt = `You are a deployment configuration expert. Analyze the repository structure and file contents provided and return a JSON object identifying the framework and deployment configuration.

You must respond with ONLY a valid JSON object — no markdown, no explanation, no backticks. The JSON must have exactly these fields:
{
  "framework": "<one of: fastapi|flask|django|python|nodejs|express|nestjs|nextjs|remix|nuxtjs|svelte|astro|react-spa|vite|go|rails|spring|static|unknown>",
  "confidence": "<high|medium|low>",
  "signals": ["<what you saw that led to this conclusion>"],
  "start_command": "<the command to start this app in production, e.g. uvicorn main:app --host 0.0.0.0 --port 8000>",
  "build_command": "<build command if needed, e.g. npm run build, else empty string>",
  "warnings": ["<any deployment concerns, e.g. hardcoded localhost URLs, missing env vars, non-standard structure>"]
}

Framework selection rules:
- react-spa: React app using react-scripts (Create React App) — no server, builds to /build, serve with Nginx
- vite: Vite-based SPA (React/Vue/Svelte without SvelteKit) — no server, builds to /dist, serve with Nginx
- static: plain HTML/CSS/JS with no build step
- svelte: SvelteKit (has @sveltejs/kit, has a server)
- If a Dockerfile exists, still identify the framework but note it in signals

For warnings, specifically check for:
- Hardcoded localhost or 127.0.0.1 URLs in source files
- Missing common required env vars (SECRET_KEY_BASE for Rails, DATABASE_URL for Django/Rails, JAVA_OPTS for Spring)
- Non-standard project structure (e.g. main package not at root for Go)
- package.json scripts that use webpack-dev-server or react-scripts start (wrong for production)`

// aiDetect is the AI fallback for framework detection. It gathers key config/source files,
// asks Claude to classify the project and infer deployment commands, and merges the result
// with the rule-based one. It never blocks the main flow: on any failure (no key, network,
// bad JSON) it returns the original ruleResult so detection degrades gracefully.
func (s *Service) aiDetect(
	ctx context.Context,
	token, owner, repo string,
	files []string,
	ruleResult *DetectResult,
) (*DetectResult, error) {
	// (a) Collect context — fetch known config files plus one source hint, ignoring errors.
	lowerToActual := make(map[string]string, len(files))
	for _, f := range files {
		lowerToActual[strings.ToLower(f)] = f
	}

	type namedFile struct {
		name    string
		content string
	}
	var collected []namedFile

	targets := []string{
		"package.json", "go.mod", "requirements.txt", "pyproject.toml",
		"Gemfile", "pom.xml", "build.gradle", "build.gradle.kts",
	}
	for _, t := range targets {
		actual, ok := lowerToActual[strings.ToLower(t)]
		if !ok {
			continue
		}
		if content, err := s.fetchFileContentDecoded(ctx, token, owner, repo, actual); err == nil {
			collected = append(collected, namedFile{name: actual, content: content})
		}
	}

	// One source hint: first *.py or main.go at root level.
	for _, f := range files {
		lf := strings.ToLower(f)
		if strings.HasSuffix(lf, ".py") || lf == "main.go" {
			if content, err := s.fetchFileContentDecoded(ctx, token, owner, repo, f); err == nil {
				collected = append(collected, namedFile{name: f, content: content})
			}
			break
		}
	}

	// (b) Build the user message.
	var sb strings.Builder
	fmt.Fprintf(&sb, "Repository: %s/%s\n\n", owner, repo)
	sb.WriteString("File tree (root level):\n")
	sb.WriteString(strings.Join(files, "\n"))
	sb.WriteString("\n\nFile contents:\n")
	for _, f := range collected {
		fmt.Fprintf(&sb, "=== %s ===\n%s\n", f.name, f.content)
	}

	// (c) Call the Anthropic API.
	rawText, err := s.llm.Complete(ctx, aiDetectSystemPrompt, sb.String(), 1024)
	if err != nil {
		return ruleResult, err
	}

	// (d) Parse the response.
	text := stripJSONFences(rawText)

	var ai struct {
		Framework    string   `json:"framework"`
		Confidence   string   `json:"confidence"`
		Signals      []string `json:"signals"`
		StartCommand string   `json:"start_command"`
		BuildCommand string   `json:"build_command"`
		Warnings     []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(text), &ai); err != nil {
		return ruleResult, fmt.Errorf("failed to parse AI detection JSON: %w", err)
	}

	// (e) Merge — AI drives framework/commands/warnings; rule signals are kept and extended.
	mergedSignals := make([]string, 0, len(ruleResult.Signals)+len(ai.Signals))
	mergedSignals = append(mergedSignals, ruleResult.Signals...)
	mergedSignals = append(mergedSignals, ai.Signals...)

	return &DetectResult{
		Framework:    ai.Framework,
		Confidence:   ai.Confidence,
		Signals:      mergedSignals,
		HasDocker:    ruleResult.HasDocker,
		StartCommand: ai.StartCommand,
		BuildCommand: ai.BuildCommand,
		Warnings:     ai.Warnings,
		AIDetected:   true,
	}, nil
}

// stripJSONFences removes accidental markdown code fences (```json ... ```) around a
// JSON payload so it can be parsed directly.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

// HandleListBranches returns branch names for a repo.
func (s *Service) HandleListBranches(c *gin.Context) {
	owner := c.Param("owner")
	repo := c.Param("repo")

	token, err := s.getTokenForUser(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "GitHub not connected"})
		return
	}

	reqURL := fmt.Sprintf("%s/repos/%s/%s/branches?per_page=100", githubAPIBase, owner, repo)
	body, err := s.doRequest(c.Request.Context(), token, reqURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch branches"})
		return
	}

	var branches []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &branches); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to parse branches"})
		return
	}

	names := make([]string, 0, len(branches))
	for _, b := range branches {
		names = append(names, b.Name)
	}
	c.JSON(http.StatusOK, names)
}

// GetLatestCommit returns the SHA and message of the latest commit.
// If branch is empty the repo's default branch is used.
func (s *Service) GetLatestCommit(ctx context.Context, token, owner, repo, branch string) (string, string, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/commits?per_page=1", githubAPIBase, owner, repo)
	if branch != "" {
		reqURL += "&sha=" + url.QueryEscape(branch)
	}
	body, err := s.doRequest(ctx, token, reqURL)
	if err != nil {
		return "", "", err
	}

	var commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}

	if err := json.Unmarshal(body, &commits); err != nil || len(commits) == 0 {
		return "", "", fmt.Errorf("no commits found")
	}

	return commits[0].SHA, commits[0].Commit.Message, nil
}

// GetTokenForDeployment returns the decrypted GitHub token for the project's owner.
// Used by the deploy service — not scoped to an HTTP context.
func (s *Service) GetTokenForDeployment(ctx context.Context, userID uuid.UUID) (string, error) {
	var encryptedToken *string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT github_token FROM users WHERE id = $1`, userID,
	).Scan(&encryptedToken)
	if err != nil {
		return "", fmt.Errorf("user not found")
	}
	if encryptedToken == nil {
		return "", fmt.Errorf("GitHub not connected for this user")
	}
	return decryptToken(*encryptedToken, s.encryptionKey, s.prevKey)
}

// ---- private helpers ----

// getTokenForUser fetches and decrypts the GitHub token for the request's authenticated user.
func (s *Service) getTokenForUser(c *gin.Context) (string, error) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		return "", fmt.Errorf("user not authenticated")
	}
	return s.GetTokenForDeployment(c.Request.Context(), userID)
}

func (s *Service) exchangeCode(code string) (string, error) {
	reqURL := fmt.Sprintf(
		"https://github.com/login/oauth/access_token?client_id=%s&client_secret=%s&code=%s",
		s.clientID, s.clientSecret, code,
	)

	req, _ := http.NewRequest("POST", reqURL, nil)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.Error != "" {
		return "", fmt.Errorf("github oauth error: %s", result.Error)
	}

	return result.AccessToken, nil
}

func (s *Service) fetchRepos(ctx context.Context, token string) ([]Repo, error) {
	body, err := s.doRequest(ctx, token, githubAPIBase+"/user/repos?per_page=100&sort=updated")
	if err != nil {
		return nil, err
	}

	var repos []Repo
	if err := json.Unmarshal(body, &repos); err != nil {
		return nil, err
	}

	return repos, nil
}

func (s *Service) listRootFiles(ctx context.Context, token, owner, repo string) ([]string, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/contents/", githubAPIBase, owner, repo)
	body, err := s.doRequest(ctx, token, reqURL)
	if err != nil {
		return nil, err
	}

	var contents []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}

	if err := json.Unmarshal(body, &contents); err != nil {
		return nil, err
	}

	var files []string
	for _, c := range contents {
		files = append(files, c.Name)
	}

	return files, nil
}

// fetchFileContentDecoded returns the decoded (plaintext) content of a file from GitHub.
func (s *Service) fetchFileContentDecoded(ctx context.Context, token, owner, repo, path string) (string, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPIBase, owner, repo, path)
	body, err := s.doRequest(ctx, token, reqURL)
	if err != nil {
		return "", err
	}

	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}

	if err := json.Unmarshal(body, &file); err != nil {
		return "", err
	}

	if file.Encoding != "base64" {
		return file.Content, nil
	}

	// GitHub wraps lines at 60 chars — strip newlines before decoding
	cleaned := strings.ReplaceAll(file.Content, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", err
	}

	return string(decoded), nil
}

func (s *Service) doRequest(ctx context.Context, token, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("github API error: %s", resp.Status)
	}

	return io.ReadAll(resp.Body)
}

// ---- crypto helpers ----

// deriveKey returns a 32-byte AES key by SHA-256 hashing the raw key string.
func deriveKey(encKey string) []byte {
	h := sha256.Sum256([]byte(encKey))
	return h[:]
}

const tokenVersion = "v1"

// encryptToken encrypts a plaintext token with AES-256-GCM and prefixes the
// result with a version tag so key rotation can be handled transparently.
// Output format: "v1:<base64(nonce + ciphertext_with_tag)>"
func encryptToken(plaintext, encKey string) (string, error) {
	key := deriveKey(encKey)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return tokenVersion + ":" + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptToken reverses encryptToken. It handles two formats:
//   - "v1:<base64>" — current format; decrypts with currentKey
//   - "<bare base64>" — legacy format (no version prefix); tries currentKey,
//     then prevKey (set via ENCRYPTION_KEY_PREV during key rotation)
func decryptToken(encrypted, currentKey, prevKey string) (string, error) {
	var payload, keyToUse string

	if strings.HasPrefix(encrypted, tokenVersion+":") {
		payload = strings.TrimPrefix(encrypted, tokenVersion+":")
		keyToUse = currentKey
	} else {
		// Legacy token — try current key first, fall back to prev key.
		payload = encrypted
		plain, err := decryptPayload(payload, currentKey)
		if err == nil {
			return plain, nil
		}
		if prevKey == "" {
			return "", fmt.Errorf("failed to decrypt legacy token (no ENCRYPTION_KEY_PREV set)")
		}
		keyToUse = prevKey
	}

	return decryptPayload(payload, keyToUse)
}

func decryptPayload(payload, encKey string) (string, error) {
	key := deriveKey(encKey)

	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("failed to decode token: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt token: %w", err)
	}

	return string(plaintext), nil
}

// generateState creates an HMAC-signed state string embedding the user ID and a timestamp.
// Format: base64url(userID|unixTs) + "." + base64url(HMAC-SHA256)
func generateState(userID uuid.UUID, encKey string) (string, error) {
	data := fmt.Sprintf("%s|%d", userID.String(), time.Now().Unix())
	dataB64 := base64.RawURLEncoding.EncodeToString([]byte(data))

	mac := hmac.New(sha256.New, deriveKey(encKey))
	mac.Write([]byte(dataB64))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return dataB64 + "." + sig, nil
}

// verifyState validates the HMAC signature, checks expiry (10 min), and returns the user ID.
func verifyState(state, encKey string) (uuid.UUID, error) {
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		return uuid.UUID{}, fmt.Errorf("invalid state format")
	}
	dataB64, sig := parts[0], parts[1]

	mac := hmac.New(sha256.New, deriveKey(encKey))
	mac.Write([]byte(dataB64))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return uuid.UUID{}, fmt.Errorf("invalid state signature")
	}

	dataBytes, err := base64.RawURLEncoding.DecodeString(dataB64)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("invalid state data")
	}

	dataParts := strings.SplitN(string(dataBytes), "|", 2)
	if len(dataParts) != 2 {
		return uuid.UUID{}, fmt.Errorf("malformed state payload")
	}

	userID, err := uuid.Parse(dataParts[0])
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("invalid user ID in state")
	}

	ts, err := strconv.ParseInt(dataParts[1], 10, 64)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("invalid timestamp in state")
	}

	if time.Since(time.Unix(ts, 0)) > 10*time.Minute {
		return uuid.UUID{}, fmt.Errorf("state expired")
	}

	return userID, nil
}

// ---- Webhook management ----

// PREvent is the subset of a GitHub pull_request webhook payload we care about.
type PREvent struct {
	Action string `json:"action"` // opened | synchronize | closed | reopened
	Number int    `json:"number"`
	PullRequest struct {
		Head struct {
			Ref string `json:"ref"` // branch name
			SHA string `json:"sha"`
		} `json:"head"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// RegisterRepoWebhook installs a pull_request webhook on the given repo.
// Returns the GitHub webhook ID so it can be stored and later deleted.
func (s *Service) RegisterRepoWebhook(ctx context.Context, token, owner, repo, webhookURL, secret string) (int64, error) {
	body, _ := json.Marshal(map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"pull_request"},
		"config": map[string]string{
			"url":          webhookURL,
			"content_type": "json",
			"secret":       secret,
			"insecure_ssl": "0",
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/repos/%s/%s/hooks", githubAPIBase, owner, repo),
		strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("github webhook registration failed (%s): %s", resp.Status, string(raw))
	}

	var hook struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &hook); err != nil {
		return 0, fmt.Errorf("failed to parse webhook response: %w", err)
	}
	return hook.ID, nil
}

// DeleteRepoWebhook removes a previously registered webhook from GitHub.
func (s *Service) DeleteRepoWebhook(ctx context.Context, token, owner, repo string, webhookID int64) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE",
		fmt.Sprintf("%s/repos/%s/%s/hooks/%d", githubAPIBase, owner, repo, webhookID),
		nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != 404 {
		return fmt.Errorf("github webhook deletion failed: %s", resp.Status)
	}
	return nil
}

// CreatePRComment posts a comment on the given PR and returns the comment ID.
func (s *Service) CreatePRComment(ctx context.Context, token, owner, repo string, prNumber int, body string) (int64, error) {
	payload, _ := json.Marshal(map[string]string{"body": body})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", githubAPIBase, owner, repo, prNumber),
		strings.NewReader(string(payload)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("create comment failed (%s): %s", resp.Status, string(raw))
	}

	var comment struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(raw, &comment)
	return comment.ID, nil
}

// UpdatePRComment replaces the body of an existing PR comment.
func (s *Service) UpdatePRComment(ctx context.Context, token, owner, repo string, commentID int64, body string) error {
	payload, _ := json.Marshal(map[string]string{"body": body})
	req, err := http.NewRequestWithContext(ctx, "PATCH",
		fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", githubAPIBase, owner, repo, commentID),
		strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("update comment failed: %s", resp.Status)
	}
	return nil
}

// VerifyWebhookSignature checks the X-Hub-Signature-256 header using the stored secret.
func VerifyWebhookSignature(payload []byte, signature, secret string) bool {
	if secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := "sha256=" + fmt.Sprintf("%x", mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}
