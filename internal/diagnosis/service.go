package diagnosis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const diagnosisPrompt = `You are an expert DevOps engineer analyzing a deployment failure or production incident.
You will be given:
1. Recent log output
2. Deployment history (what changed)
3. Past incidents for this project (memory layer)

Your job:
- Identify the root cause clearly and specifically
- Reference the exact log line or code change that caused it
- Provide a concrete fix (specific env var to add, config to change, code to fix)
- Be direct and concise — no filler

Format your response as:
**Root Cause:** <one sentence>
**Evidence:** <specific log line or change>
**Fix:** <specific actionable step>
**Prevention:** <one line>`

type Service struct {
	db     *models.DB
	awsSvc *aws.Service
	apiKey string
}

func NewService(db *models.DB, awsSvc *aws.Service, apiKey string) *Service {
	return &Service{db: db, awsSvc: awsSvc, apiKey: apiKey}
}

// HandleDiagnose is the HTTP handler for diagnosis requests
func (s *Service) HandleDiagnose(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	result, err := s.DiagnoseProject(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"diagnosis": result})
}

// DiagnoseProject collects context and sends to Claude for analysis
func (s *Service) DiagnoseProject(ctx context.Context, projectID uuid.UUID) (string, error) {
	// Step 1: fetch last failed deployment
	deployment, err := s.getLastFailedDeployment(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("no failed deployment found: %w", err)
	}

	// Step 2: fetch recent deployments for diff context
	history, err := s.getDeploymentHistory(ctx, projectID, 5)
	if err != nil {
		history = "Unable to fetch deployment history."
	}

	// Step 3: fetch past incidents (memory layer)
	pastIncidents, err := s.getPastIncidents(ctx, projectID, 3)
	if err != nil {
		pastIncidents = "No past incidents on record."
	}

	// Step 4: fetch logs
	// TODO: get environment → assume role → fetch CloudWatch logs
	rawLogs := "Log collection not yet implemented — connect AWS first."
	if deployment.FailureReason != nil {
		rawLogs = *deployment.FailureReason
	}

	// Step 5: send to Claude
	diagnosis, err := s.analyzeWithClaude(ctx, rawLogs, history, pastIncidents)
	if err != nil {
		return "", fmt.Errorf("analysis failed: %w", err)
	}

	// Step 6: save incident to memory layer
	s.saveIncident(ctx, projectID, deployment.ID, "deploy_failure", rawLogs, diagnosis)

	return diagnosis, nil
}

func (s *Service) analyzeWithClaude(ctx context.Context, logs, history, pastIncidents string) (string, error) {
	userMessage := fmt.Sprintf(
		"## Logs\n%s\n\n## Deployment History\n%s\n\n## Past Incidents\n%s",
		logs, history, pastIncidents,
	)

	reqBody := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 1000,
		"system":     diagnosisPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userMessage},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var claudeResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.Unmarshal(respBody, &claudeResp); err != nil {
		return "", err
	}

	if len(claudeResp.Content) == 0 {
		return "", fmt.Errorf("empty response from Claude")
	}

	return claudeResp.Content[0].Text, nil
}

func (s *Service) getLastFailedDeployment(ctx context.Context, projectID uuid.UUID) (*models.Deployment, error) {
	var d models.Deployment
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, project_id, environment_id, commit_sha, status, failure_reason, created_at
		 FROM deployments WHERE project_id = $1 AND status = 'failed'
		 ORDER BY created_at DESC LIMIT 1`, projectID,
	).Scan(&d.ID, &d.ProjectID, &d.EnvironmentID, &d.CommitSHA, &d.Status, &d.FailureReason, &d.CreatedAt)

	return &d, err
}

func (s *Service) getDeploymentHistory(ctx context.Context, projectID uuid.UUID, limit int) (string, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT commit_sha, status, created_at FROM deployments
		 WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2`,
		projectID, limit,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var sha, status, createdAt string
		if err := rows.Scan(&sha, &status, &createdAt); err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s — %s — %s", sha[:8], status, createdAt))
	}

	return strings.Join(lines, "\n"), nil
}

func (s *Service) getPastIncidents(ctx context.Context, projectID uuid.UUID, limit int) (string, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT root_cause, resolution, created_at FROM incidents
		 WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2`,
		projectID, limit,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var rootCause, resolution, createdAt string
		if err := rows.Scan(&rootCause, &resolution, &createdAt); err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s] Cause: %s | Fix: %s", createdAt, rootCause, resolution))
	}

	if len(lines) == 0 {
		return "No past incidents on record.", nil
	}

	return strings.Join(lines, "\n"), nil
}

func (s *Service) saveIncident(ctx context.Context, projectID, deploymentID uuid.UUID, trigger, logs, diagnosis string) {
	s.db.Pool.Exec(ctx,
		`INSERT INTO incidents (project_id, deployment_id, trigger, root_cause, raw_logs)
		 VALUES ($1, $2, $3, $4, $5)`,
		projectID, deploymentID, trigger, diagnosis, logs,
	)
}
