package diagnosis

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/internal/memory"
	"github.com/ashborntechnologies-web/OpsPilot/internal/prompts"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// maxLogChars caps the application-log slice sent to Claude to control token usage.
const maxLogChars = 6000

type Service struct {
	db     *models.DB
	awsSvc *aws.Service
	events *events.Service
	llm    *llm.Client
	memory *memory.Service
}

// SetMemoryService injects the project-memory service (set once at startup).
// Diagnosis works without it; memory just makes prompts smarter over time.
func (s *Service) SetMemoryService(m *memory.Service) { s.memory = m }

func NewService(db *models.DB, awsSvc *aws.Service, eventSvc *events.Service, apiKey string) *Service {
	return &Service{db: db, awsSvc: awsSvc, events: eventSvc, llm: llm.New(apiKey)}
}

// HandleDiagnose is the HTTP handler for diagnosis requests. It diagnoses the
// specific deployment in the URL — not just the most recent failure — so the
// result matches the row the user clicked "Diagnose" on.
func (s *Service) HandleDiagnose(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	deploymentID, err := uuid.Parse(c.Param("deployId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deployment id"})
		return
	}

	deployment, err := s.getDeployment(c.Request.Context(), projectID, deploymentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment not found"})
		return
	}

	result, incidentID, err := s.diagnose(c.Request.Context(), projectID, deployment)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"diagnosis": result, "incident_id": incidentID})
}

// DiagnoseProject diagnoses the most recent failed deployment — the entrypoint
// for conversational "why did it fail?" requests where no specific deployment
// is referenced.
func (s *Service) DiagnoseProject(ctx context.Context, projectID uuid.UUID) (string, error) {
	deployment, err := s.getLastFailedDeployment(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("no failed deployment found: %w", err)
	}
	result, _, err := s.diagnose(ctx, projectID, deployment)
	return result, err
}

// diagnose collects real operational context (failure reason, live CloudWatch logs,
// the deployment's event timeline, deployment history, and past incidents) for one
// deployment and sends a structured, size-bounded prompt to Claude. Missing logs are
// handled gracefully.
func (s *Service) diagnose(ctx context.Context, projectID uuid.UUID, deployment *models.Deployment) (string, uuid.UUID, error) {
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

	// Step 6.5: project memory — facts learned from this project's history
	memorySection := s.memorySection(ctx, projectID)

	// Step 7: assemble bounded context and send to Claude
	userMessage := buildDiagnosisContext(failureReason, logLines, history, pastIncidents, eventTimeline)
	if memorySection != "" {
		userMessage += "\n\n" + memorySection
	}
	diagnosis, err := s.analyzeWithClaude(ctx, userMessage)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("analysis failed: %w", err)
	}

	// Step 8: save incident to memory layer
	rawForMemory := failureReason
	if len(logLines) > 0 {
		rawForMemory = strings.Join(logLines, "\n")
	}
	incidentID := s.saveIncident(ctx, projectID, deployment.ID, "deploy_failure", rawForMemory, diagnosis)

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

	return diagnosis, incidentID, nil
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
	return s.llm.Complete(ctx, prompts.Diagnosis(), userMessage, 1000)
}

// getDeployment loads one deployment scoped to the project (tenant-isolation guard).
func (s *Service) getDeployment(ctx context.Context, projectID, deploymentID uuid.UUID) (*models.Deployment, error) {
	var d models.Deployment
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, project_id, environment_id, commit_sha, status, failure_reason, created_at
		 FROM deployments WHERE id = $1 AND project_id = $2`, deploymentID, projectID,
	).Scan(&d.ID, &d.ProjectID, &d.EnvironmentID, &d.CommitSHA, &d.Status, &d.FailureReason, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
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
		`SELECT root_cause, COALESCE(resolution, ''), created_at FROM incidents
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

// extractDiagnosisField pulls a single "**Label:** value" line out of the
// structured diagnosis response (see diagnosisPrompt's response format).
func extractDiagnosisField(diagnosis, label string) string {
	marker := "**" + label + ":**"
	idx := strings.Index(diagnosis, marker)
	if idx == -1 {
		return ""
	}
	rest := diagnosis[idx+len(marker):]
	if nl := strings.Index(rest, "\n"); nl != -1 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest)
}

func (s *Service) saveIncident(ctx context.Context, projectID, deploymentID uuid.UUID, trigger, logs, diagnosis string) uuid.UUID {
	// Store the structured pieces so the memory layer can show past causes AND
	// their fixes; fall back to the full text when parsing fails.
	rootCause := extractDiagnosisField(diagnosis, "Root Cause")
	if rootCause == "" {
		rootCause = diagnosis
	}
	resolution := extractDiagnosisField(diagnosis, "Fix")

	var id uuid.UUID
	s.db.Pool.QueryRow(ctx,
		`INSERT INTO incidents (project_id, deployment_id, trigger, root_cause, resolution, raw_logs)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		projectID, deploymentID, trigger, rootCause, resolution, logs,
	).Scan(&id)
	return id
}

// memorySection renders the project's learned memory for prompt injection.
func (s *Service) memorySection(ctx context.Context, projectID uuid.UUID) string {
	if s.memory == nil {
		return ""
	}
	memories, err := s.memory.GetRelevantMemory(ctx, projectID, 5)
	if err != nil {
		return ""
	}
	return memory.FormatForPrompt(memories)
}

// DiagnoseRuntime analyzes the current runtime state of a project without a
// specific failed deployment — triggered autonomously when the monitoring
// subsystem emits an error-severity runtime anomaly. Context is built from
// recent logs, the latest operational events, project memory, and past
// incidents. The result is stored as a runtime_anomaly incident and the most
// recent open alert's summary is refreshed with the diagnosis headline.
func (s *Service) DiagnoseRuntime(ctx context.Context, projectID uuid.UUID) (string, error) {
	// Most recently active ready environment for the project.
	var env models.Environment
	err := s.db.Pool.QueryRow(ctx, `
		SELECT id, project_id, account_id, aws_region, log_group_name
		FROM environments
		WHERE project_id = $1 AND stack_status = 'ready' AND is_preview = false
		ORDER BY updated_at DESC LIMIT 1`, projectID,
	).Scan(&env.ID, &env.ProjectID, &env.AccountID, &env.AWSRegion, &env.LogGroupName)
	if err != nil {
		return "", fmt.Errorf("no ready environment to diagnose: %w", err)
	}

	// Recent application logs (best-effort).
	var logLines []string
	if env.LogGroupName != nil && env.AccountID != nil {
		if clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, &env); err == nil {
			logLines, _ = s.awsSvc.FetchRecentECSLogs(ctx, clients, *env.LogGroupName, 200)
		}
	}

	// Last 10 operational events for this environment.
	eventTimeline := s.getEnvEventTimeline(ctx, env.ID, 10)

	pastIncidents, err := s.getPastIncidents(ctx, projectID, 3)
	if err != nil {
		pastIncidents = "No past incidents on record."
	}
	memorySection := s.memorySection(ctx, projectID)

	userMessage := "# Runtime Anomaly Analysis\nNo deployment is in flight — the running service started misbehaving.\n\n" +
		buildDiagnosisContext("Runtime anomaly detected by continuous monitoring (no recorded deploy failure).",
			logLines, "Not applicable — no recent deployment triggered this.", pastIncidents, eventTimeline)
	if memorySection != "" {
		userMessage += "\n\n" + memorySection
	}

	envID := env.ID
	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: &envID,
		Type:          models.EventDiagnosisStarted,
		Source:        models.SourceAI,
		ActorType:     models.ActorAI,
		Payload:       map[string]any{"trigger": "runtime_anomaly"},
	})

	diagnosis, err := s.analyzeWithClaude(ctx, userMessage)
	if err != nil {
		return "", fmt.Errorf("runtime analysis failed: %w", err)
	}

	rawForMemory := ""
	if len(logLines) > 0 {
		rawForMemory = strings.Join(logLines, "\n")
	}

	// Store as a runtime_anomaly incident (no deployment attached).
	rootCause := extractDiagnosisField(diagnosis, "Root Cause")
	if rootCause == "" {
		rootCause = diagnosis
	}
	resolution := extractDiagnosisField(diagnosis, "Fix")
	var incidentID uuid.UUID
	s.db.Pool.QueryRow(ctx,
		`INSERT INTO incidents (project_id, environment_id, trigger, root_cause, resolution, raw_logs)
		 VALUES ($1, $2, 'runtime_anomaly', $3, $4, $5) RETURNING id`,
		projectID, envID, rootCause, resolution, rawForMemory,
	).Scan(&incidentID)

	// Refresh the latest open alert with the diagnosis headline so the alert
	// panel shows the root cause, not just the symptom.
	if headline := firstSentence(rootCause); headline != "" {
		s.db.Pool.Exec(ctx, `
			UPDATE alerts SET summary = $1
			WHERE id = (SELECT id FROM alerts WHERE project_id = $2 AND status = 'open'
			            ORDER BY triggered_at DESC LIMIT 1)`,
			headline, projectID)
	}

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: &envID,
		Type:          models.EventDiagnosisCompleted,
		Source:        models.SourceAI,
		ActorType:     models.ActorAI,
		Payload:       map[string]any{"trigger": "runtime_anomaly", "incident_id": incidentID.String()},
	})

	return diagnosis, nil
}

// getEnvEventTimeline renders the last N operational events for an environment.
func (s *Service) getEnvEventTimeline(ctx context.Context, envID uuid.UUID, limit int) string {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT event_type, severity, occurred_at FROM operational_events
		WHERE environment_id = $1 ORDER BY occurred_at DESC LIMIT $2`, envID, limit)
	if err != nil {
		return "No event timeline available."
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var eventType, severity string
		var occurredAt time.Time
		if rows.Scan(&eventType, &severity, &occurredAt) != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s [%s] %s", occurredAt.Format(time.RFC3339), severity, eventType))
	}
	if len(lines) == 0 {
		return "No event timeline available."
	}
	return strings.Join(lines, "\n")
}

// firstSentence returns the text up to the first period (or the whole string).
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i != -1 {
		return s[:i+1]
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
