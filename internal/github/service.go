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
	db      *models.DB
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
}

func NewService(clientID, clientSecret, redirectURL, encryptionKey string, db *models.DB) *Service {
	return &Service{
		clientID:      clientID,
		clientSecret:  clientSecret,
		redirectURL:   redirectURL,
		encryptionKey: encryptionKey,
		prevKey:       os.Getenv("ENCRYPTION_KEY_PREV"),
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

	return result, nil
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
