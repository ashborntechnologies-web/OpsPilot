package framework

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GithubClient manages test repositories via the GitHub REST API.
// It creates repos if they don't exist, pushes file trees, and deletes them on cleanup.
type GithubClient struct {
	token string
	http  *http.Client
}

func NewGithubClient(token string) *GithubClient {
	return &GithubClient{
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *GithubClient) gh(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, reqBody)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

// EnsureRepo creates the repo under `owner` if it does not already exist.
// Returns the clone URL.
func (g *GithubClient) EnsureRepo(ctx context.Context, owner, name, description string) (string, error) {
	// Check if it exists
	_, status, err := g.gh(ctx, "GET", fmt.Sprintf("/repos/%s/%s", owner, name), nil)
	if err != nil {
		return "", fmt.Errorf("check repo: %w", err)
	}
	if status == 200 {
		return fmt.Sprintf("https://github.com/%s/%s", owner, name), nil
	}

	// Create under user account (works for both user and org tokens)
	payload := map[string]interface{}{
		"name":        name,
		"description": description,
		"private":     false,
		"auto_init":   true,
	}

	// Try org endpoint first, fall back to user endpoint
	data, status, err := g.gh(ctx, "POST", fmt.Sprintf("/orgs/%s/repos", owner), payload)
	if err != nil || status >= 400 {
		data, status, err = g.gh(ctx, "POST", "/user/repos", payload)
	}
	if err != nil {
		return "", fmt.Errorf("create repo: %w", err)
	}
	if status != 201 {
		return "", fmt.Errorf("create repo HTTP %d: %s", status, string(data))
	}

	// Wait for the default branch to be ready
	time.Sleep(2 * time.Second)
	return fmt.Sprintf("https://github.com/%s/%s", owner, name), nil
}

// PushFiles commits a set of files to the repo's default branch using the Git Data API.
// files is a map of path → file content.
func (g *GithubClient) PushFiles(ctx context.Context, owner, repo string, files map[string]string, message string) error {
	// Get the default branch ref
	refData, status, err := g.gh(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/refs/heads/main", owner, repo), nil)
	if err != nil || status != 200 {
		// Try master
		refData, status, err = g.gh(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/refs/heads/master", owner, repo), nil)
		if err != nil {
			return fmt.Errorf("get ref: %w", err)
		}
	}
	if status != 200 {
		return fmt.Errorf("get ref HTTP %d: %s", status, string(refData))
	}

	var ref struct {
		Object struct{ SHA string `json:"sha"` } `json:"object"`
	}
	json.Unmarshal(refData, &ref)
	baseSHA := ref.Object.SHA

	// Get base tree SHA
	commitData, _, _ := g.gh(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/commits/%s", owner, repo, baseSHA), nil)
	var commit struct {
		Tree struct{ SHA string `json:"sha"` } `json:"tree"`
	}
	json.Unmarshal(commitData, &commit)

	// Create blobs for each file
	type treeEntry struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	var treeEntries []treeEntry

	for path, content := range files {
		blobPayload := map[string]string{
			"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			"encoding": "base64",
		}
		blobData, blobStatus, err := g.gh(ctx, "POST",
			fmt.Sprintf("/repos/%s/%s/git/blobs", owner, repo), blobPayload)
		if err != nil || blobStatus != 201 {
			return fmt.Errorf("create blob for %s: status %d %v", path, blobStatus, err)
		}
		var blob struct{ SHA string `json:"sha"` }
		json.Unmarshal(blobData, &blob)
		treeEntries = append(treeEntries, treeEntry{
			Path: path, Mode: "100644", Type: "blob", SHA: blob.SHA,
		})
	}

	// Create tree
	treePayload := map[string]interface{}{
		"base_tree": commit.Tree.SHA,
		"tree":      treeEntries,
	}
	treeData, treeStatus, err := g.gh(ctx, "POST",
		fmt.Sprintf("/repos/%s/%s/git/trees", owner, repo), treePayload)
	if err != nil || treeStatus != 201 {
		return fmt.Errorf("create tree: HTTP %d %v", treeStatus, err)
	}
	var tree struct{ SHA string `json:"sha"` }
	json.Unmarshal(treeData, &tree)

	// Create commit
	newCommitPayload := map[string]interface{}{
		"message": message,
		"tree":    tree.SHA,
		"parents": []string{baseSHA},
	}
	newCommitData, newCommitStatus, err := g.gh(ctx, "POST",
		fmt.Sprintf("/repos/%s/%s/git/commits", owner, repo), newCommitPayload)
	if err != nil || newCommitStatus != 201 {
		return fmt.Errorf("create commit: HTTP %d %v", newCommitStatus, err)
	}
	var newCommit struct{ SHA string `json:"sha"` }
	json.Unmarshal(newCommitData, &newCommit)

	// Update ref — determine branch name from the ref that worked
	branch := "main"
	if strings.Contains(string(refData), "heads/master") {
		branch = "master"
	}
	updateRefPayload := map[string]interface{}{
		"sha":   newCommit.SHA,
		"force": false,
	}
	_, updateStatus, err := g.gh(ctx, "PATCH",
		fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, repo, branch), updateRefPayload)
	if err != nil || updateStatus != 200 {
		return fmt.Errorf("update ref: HTTP %d %v", updateStatus, err)
	}

	return nil
}

// GetDefaultBranch returns the default branch name of a repo.
func (g *GithubClient) GetDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	data, status, err := g.gh(ctx, "GET", fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	if err != nil || status != 200 {
		return "main", nil // sensible default
	}
	var r struct{ DefaultBranch string `json:"default_branch"` }
	json.Unmarshal(data, &r)
	if r.DefaultBranch == "" {
		return "main", nil
	}
	return r.DefaultBranch, nil
}

// DeleteRepo deletes the repository — used during cleanup.
func (g *GithubClient) DeleteRepo(ctx context.Context, owner, repo string) error {
	_, status, err := g.gh(ctx, "DELETE", fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return err
	}
	if status != 204 {
		return fmt.Errorf("delete repo HTTP %d", status)
	}
	return nil
}
