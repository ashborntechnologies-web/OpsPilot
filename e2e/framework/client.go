package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client wraps all OpsPilot API calls made during E2E tests.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ── API response types ────────────────────────────────────────────────────────

type Project struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	RepoOwner    string  `json:"repo_owner"`
	RepoName     string  `json:"repo_name"`
	Framework    string  `json:"framework"`
	AccountID    *string `json:"account_id"`
}

type AWSAccount struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	AWSAccountID string `json:"aws_account_id"`
	IAMRoleARN   string `json:"iam_role_arn"`
}

type Environment struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"project_id"`
	Name        string  `json:"name"`
	StackStatus string  `json:"stack_status"`
	ALBDNS      *string `json:"alb_dns"`
	IsPreview   bool    `json:"is_preview"`
}

type Deployment struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	CommitSHA     string  `json:"commit_sha"`
	CommitMessage *string `json:"commit_message"`
	FailureReason *string `json:"failure_reason"`
	CreatedAt     string  `json:"created_at"`
}

type HealthResponse struct {
	Status  string  `json:"status"`
	Running int     `json:"running"`
	Desired int     `json:"desired"`
	URL     *string `json:"url"`
}

// ── Core request helper ────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, reqBody)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out interface{}) error {
	data, status, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		var errBody struct{ Error string `json:"error"` }
		json.Unmarshal(data, &errBody)
		if errBody.Error != "" {
			return fmt.Errorf("HTTP %d: %s", status, errBody.Error)
		}
		return fmt.Errorf("HTTP %d", status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ── AWS Accounts ──────────────────────────────────────────────────────────────

func (c *Client) ConnectAWSAccount(ctx context.Context, label, accountID, roleARN, externalID string) (*AWSAccount, error) {
	var out AWSAccount
	err := c.doJSON(ctx, "POST", "/aws-accounts", map[string]string{
		"label":          label,
		"aws_account_id": accountID,
		"iam_role_arn":   roleARN,
		"external_id":    externalID,
	}, &out)
	return &out, err
}

func (c *Client) ListAWSAccounts(ctx context.Context) ([]AWSAccount, error) {
	var out []AWSAccount
	return out, c.doJSON(ctx, "GET", "/aws-accounts", nil, &out)
}

func (c *Client) DeleteAWSAccount(ctx context.Context, id string) error {
	_, status, err := c.do(ctx, "DELETE", "/aws-accounts/"+id, nil)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("delete account HTTP %d", status)
	}
	return nil
}

// ── Projects ──────────────────────────────────────────────────────────────────

type CreateProjectRequest struct {
	Name         string  `json:"name"`
	RepoURL      string  `json:"repo_url"`
	RepoOwner    string  `json:"repo_owner"`
	RepoName     string  `json:"repo_name"`
	Framework    string  `json:"framework"`
	Branch       *string `json:"branch,omitempty"`
	StartCommand *string `json:"start_command,omitempty"`
	AccountID    *string `json:"account_id,omitempty"`
}

func (c *Client) CreateProject(ctx context.Context, req CreateProjectRequest) (*Project, error) {
	var out Project
	return &out, c.doJSON(ctx, "POST", "/projects", req, &out)
}

func (c *Client) GetProject(ctx context.Context, projectID string) (*Project, error) {
	var out Project
	return &out, c.doJSON(ctx, "GET", "/projects/"+projectID, nil, &out)
}

func (c *Client) DeleteProject(ctx context.Context, projectID string) error {
	_, status, err := c.do(ctx, "DELETE", "/projects/"+projectID, nil)
	if err != nil {
		return err
	}
	if status != 202 && status != 200 {
		return fmt.Errorf("delete project HTTP %d", status)
	}
	return nil
}

// ── Environments ──────────────────────────────────────────────────────────────

func (c *Client) CreateEnvironment(ctx context.Context, projectID, name, region string) (*Environment, error) {
	var out Environment
	return &out, c.doJSON(ctx, "POST", "/projects/"+projectID+"/environments", map[string]string{
		"name":       name,
		"aws_region": region,
	}, &out)
}

func (c *Client) ListEnvironments(ctx context.Context, projectID string) ([]Environment, error) {
	var out []Environment
	return out, c.doJSON(ctx, "GET", "/projects/"+projectID+"/environments", nil, &out)
}

// WaitForEnvironment polls until the environment reaches "ready" or "failed", or the context is cancelled.
func (c *Client) WaitForEnvironment(ctx context.Context, projectID, envID string, log func(string)) (*Environment, error) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		envs, err := c.ListEnvironments(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("list environments: %w", err)
		}
		for _, e := range envs {
			if e.ID != envID {
				continue
			}
			switch e.StackStatus {
			case "ready":
				return &e, nil
			case "failed":
				return nil, fmt.Errorf("environment provisioning failed")
			default:
				if log != nil {
					log(fmt.Sprintf("env status: %s", e.StackStatus))
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for environment: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// ── Deployments ───────────────────────────────────────────────────────────────

func (c *Client) TriggerDeploy(ctx context.Context, projectID, envName string) error {
	_, status, err := c.do(ctx, "POST",
		"/projects/"+projectID+"/environments/"+envName+"/deploy?env="+envName, nil)
	// This endpoint takes the envId in the path but env name as query param.
	// Actually looking at the handler it takes ?env=<name> but the path has :envId.
	// We'll use the right path below.
	_ = status
	return err
}

// TriggerDeployByEnvID triggers a deploy for a specific environment ID.
func (c *Client) TriggerDeployByEnvID(ctx context.Context, projectID, envID, envName string) error {
	_, status, err := c.do(ctx, "POST",
		fmt.Sprintf("/projects/%s/environments/%s/deploy?env=%s", projectID, envID, envName), nil)
	if err != nil {
		return err
	}
	if status != 202 && status != 200 {
		return fmt.Errorf("trigger deploy HTTP %d", status)
	}
	return nil
}

func (c *Client) ListDeployments(ctx context.Context, projectID string) ([]Deployment, error) {
	var out []Deployment
	return out, c.doJSON(ctx, "GET", "/projects/"+projectID+"/deployments", nil, &out)
}

// WaitForDeployment polls until the most-recent deployment reaches a terminal state.
func (c *Client) WaitForDeployment(ctx context.Context, projectID string, log func(string)) (*Deployment, error) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		deps, err := c.ListDeployments(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("list deployments: %w", err)
		}
		if len(deps) > 0 {
			d := deps[0] // most recent first
			switch d.Status {
			case "live":
				return &d, nil
			case "failed", "rolled_back":
				reason := ""
				if d.FailureReason != nil {
					reason = *d.FailureReason
				}
				return &d, fmt.Errorf("deployment %s: %s", d.Status, reason)
			default:
				if log != nil {
					log(fmt.Sprintf("deploy status: %s", d.Status))
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for deployment: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Client) Rollback(ctx context.Context, projectID, deploymentID string) error {
	_, status, err := c.do(ctx, "POST",
		fmt.Sprintf("/projects/%s/deployments/%s/rollback", projectID, deploymentID), nil)
	if err != nil {
		return err
	}
	if status != 200 && status != 202 {
		return fmt.Errorf("rollback HTTP %d", status)
	}
	return nil
}

func (c *Client) DiagnoseDeployment(ctx context.Context, projectID, deploymentID string) (string, error) {
	var out struct {
		Diagnosis string `json:"diagnosis"`
	}
	err := c.doJSON(ctx, "GET",
		fmt.Sprintf("/projects/%s/deployments/%s/diagnose", projectID, deploymentID),
		nil, &out)
	return out.Diagnosis, err
}

// ── Health ────────────────────────────────────────────────────────────────────

func (c *Client) GetEnvHealth(ctx context.Context, projectID, envID string) (*HealthResponse, error) {
	var out HealthResponse
	return &out, c.doJSON(ctx, "GET",
		fmt.Sprintf("/projects/%s/environments/%s/health", projectID, envID),
		nil, &out)
}

// VerifyHTTP checks that the live URL returns HTTP 200.
func (c *Client) VerifyHTTP(ctx context.Context, liveURL, healthPath string) error {
	url := strings.TrimRight(liveURL, "/") + healthPath
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned HTTP %d", url, resp.StatusCode)
	}
	return nil
}

// ── Env Vars ──────────────────────────────────────────────────────────────────

func (c *Client) UpsertEnvVar(ctx context.Context, projectID, envID, key, value string, secret bool) error {
	return c.doJSON(ctx, "PUT",
		fmt.Sprintf("/projects/%s/environments/%s/env-vars", projectID, envID),
		map[string]interface{}{"key": key, "value": value, "is_secret": secret},
		nil)
}

// ── Logs ──────────────────────────────────────────────────────────────────────

func (c *Client) GetLogs(ctx context.Context, projectID, envID string, lines int) ([]string, error) {
	var out struct{ Lines []string `json:"lines"` }
	err := c.doJSON(ctx, "GET",
		fmt.Sprintf("/projects/%s/environments/%s/logs?lines=%d", projectID, envID, lines),
		nil, &out)
	return out.Lines, err
}
