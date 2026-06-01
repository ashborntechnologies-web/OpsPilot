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
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const diagnosisPrompt = `You are an expert DevOps engineer analyzing a deployment failure or production incident.
You will be given:
1. The recorded failure reason
2. Recent application log output (from CloudWatch)
3. The deployment's operational event timeline
4. Deployment history (what changed)
5. Past incidents for this project (memory layer)

Your job:
- Identify the root cause clearly and specifically
- Reference the exact log line or change that caused it
- Provide a concrete fix (specific env var to add, config to change, code to fix)
- State how confident you are, based on how directly the evidence supports the cause
- Be direct and concise — no filler

Format your response EXACTLY as:
**Root Cause:** <one sentence>
**Evidence:** <specific log line or change>
**Fix:** <specific actionable step>
**Prevention:** <one line>
**Confidence:** <High|Medium|Low>`

// maxLogChars caps the application-log slice sent to Claude to control token usage.
const maxLogChars = 6000

type Service struct {
	db     *models.DB
	awsSvc *aws.Service
	events *events.Service
	apiKey string
}

func NewService(db *models.DB, awsSvc *aws.Service, eventSvc *events.Service, apiKey string) *Service {
	return &Service{db: db, awsSvc: awsSvc, events: eventSvc, apiKey: apiKey}
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

// DiagnoseProject collects real operational context (failure reason, live CloudWatch logs,
// the deployment's event timeline, deployment history, and past incidents) and sends a
// structured, size-bounded prompt to Claude. Missing logs are handled gracefully.
func (s *Service) DiagnoseProject(ctx context.Context, projectID uuid.UUID) (string, error) {
	// Step 1: fetch last failed deployment
	deployment, err := s.getLastFailedDeployment(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("no failed deployment found: %w", err)
	}

	if s.events != nil {
		depID := deployment.ID
		envID := deployment.EnvironmentID
		s.events.Emit(ctx, events.Event{
			ProjectID:     projectID,
			EnvironmentID: &envID,
			DeploymentID:  &depID,
			Type:          models.EventDiagnosisStarted,
			Source:        models.SourceAI,
			ActorType:     models.ActorAI,
			Payload:       map[string]any{"commit_sha": deployment.CommitSHA},
		})
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

	// Step 4: failure reason (often carries the build-log tail for build failures)
	failureReason := ""
	if deployment.FailureReason != nil {
		failureReason = *deployment.FailureReason
	}

	// Step 5: live application logs from CloudWatch (best-effort — graceful if unavailable)
	logLines := s.fetchDeploymentLogs(ctx, deployment)

	// Step 6: the deployment's operational event timeline
	eventTimeline := s.getEventTimeline(ctx, deployment.ID)

	// Step 7: assemble bounded context and send to Claude
	userMessage := buildDiagnosisContext(failureReason, logLines, history, pastIncidents, eventTimeline)
	diagnosis, err := s.analyzeWithClaude(ctx, userMessage)
	if err != nil {
		return "", fmt.Errorf("analysis failed: %w", err)
	}

	// Step 8: save incident to memory layer
	rawForMemory := failureReason
	if len(logLines) > 0 {
		rawForMemory = strings.Join(logLines, "\n")
	}
	s.saveIncident(ctx, projectID, deployment.ID, "deploy_failure", rawForMemory, diagnosis)

	if s.events != nil {
		depID := deployment.ID
		envID := deployment.EnvironmentID
		s.events.Emit(ctx, events.Event{
			ProjectID:     projectID,
			EnvironmentID: &envID,
			DeploymentID:  &depID,
			Type:          models.EventDiagnosisCompleted,
			Source:        models.SourceAI,
			ActorType:     models.ActorAI,
			Payload:       map[string]any{"log_lines": len(logLines), "had_logs": len(logLines) > 0},
		})
	}

	return diagnosis, nil
}

// fetchDeploymentLogs loads the environment for a deployment, assumes its IAM role, and
// pulls recent ECS application logs from CloudWatch. Best-effort: returns nil on any error
// (no environment, not provisioned, assume-role failure) so diagnosis degrades gracefully.
func (s *Service) fetchDeploymentLogs(ctx context.Context, deployment *models.Deployment) []string {
	var env models.Environment
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, account_id, aws_region, log_group_name FROM environments WHERE id = $1`,
		deployment.EnvironmentID,
	).Scan(&env.ID, &env.AccountID, &env.AWSRegion, &env.LogGroupName)
	if err != nil || env.LogGroupName == nil || env.AccountID == nil {
		return nil
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, &env)
	if err != nil {
		return nil
	}

	lines, err := s.awsSvc.FetchRecentECSLogs(ctx, clients, *env.LogGroupName, 100)
	if err != nil {
		return nil
	}
	return lines
}

// getEventTimeline returns a compact text rendering of the deployment's operational events.
func (s *Service) getEventTimeline(ctx context.Context, deploymentID uuid.UUID) string {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT event_type, severity, occurred_at FROM operational_events
		 WHERE deployment_id = $1 ORDER BY occurred_at ASC`, deploymentID)
	if err != nil {
		return "No event timeline available."
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var eventType, severity, occurredAt string
		if err := rows.Scan(&eventType, &severity, &occurredAt); err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s [%s] %s", occurredAt, severity, eventType))
	}
	if len(lines) == 0 {
		return "No event timeline available."
	}
	return strings.Join(lines, "\n")
}

// buildDiagnosisContext assembles the user-message context for Claude, bounding the log
// slice to maxLogChars (keeping the most recent lines) to control token usage. Pure function
// for testability.
func buildDiagnosisContext(failureReason string, logLines []string, history, pastIncidents, eventTimeline string) string {
	logSection := "No application logs available (environment may not be provisioned or has no recent output)."
	if len(logLines) > 0 {
		joined := strings.Join(logLines, "\n")
		if len(joined) > maxLogChars {
			// Keep the tail — the most recent output is the most diagnostic.
			joined = "...(truncated)...\n" + joined[len(joined)-maxLogChars:]
		}
		logSection = joined
	}

	if strings.TrimSpace(failureReason) == "" {
		failureReason = "No recorded failure reason."
	}
	if strings.TrimSpace(eventTimeline) == "" {
		eventTimeline = "No event timeline available."
	}

	return fmt.Sprintf(
		"## Failure Reason\n%s\n\n## Application Logs\n%s\n\n## Event Timeline\n%s\n\n## Deployment History\n%s\n\n## Past Incidents\n%s",
		failureReason, logSection, eventTimeline, history, pastIncidents,
	)
}

func (s *Service) analyzeWithClaude(ctx context.Context, userMessage string) (string, error) {
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
