// alerts.go is the alert engine: it turns raw operational events into
// deduplicated, snooze-aware, AI-summarized alerts that users actually see.
// Every emitted OperationalEvent flows through EvaluateEvent (hooked into
// events.Service); recovery events resolve matching open alerts.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/internal/notify"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/google/uuid"
)

const alertDedupWindow = 30 * time.Minute

// AlertEngine evaluates operational events and manages the alert lifecycle.
type AlertEngine struct {
	db       *models.DB
	llm      *llm.Client
	hub      *ws.Hub
	emailSvc *notify.EmailService
}

func NewAlertEngine(db *models.DB, llmClient *llm.Client, hub *ws.Hub, emailSvc *notify.EmailService) *AlertEngine {
	return &AlertEngine{db: db, llm: llmClient, hub: hub, emailSvc: emailSvc}
}

// MapEventToAlert returns the alert type for an operational event, or "" when
// the event does not produce an alert. Pure function — unit tested.
func MapEventToAlert(eventType string, payload map[string]any) string {
	switch eventType {
	case models.EventRuntimeServiceDown:
		return models.AlertTypeServiceDown
	case models.EventRuntimeTasksDegraded:
		return models.AlertTypeTasksDegraded
	case models.EventRuntimeHighErrorRate:
		return models.AlertTypeHighErrorRate
	case models.EventRuntimeHighLatency:
		return models.AlertTypeHighLatency
	case models.EventRuntimeLogAnomaly:
		if pt, _ := payload["pattern_type"].(string); pt == PatternCrashLoop {
			return models.AlertTypeCrashLoop
		}
		return models.AlertTypeLogAnomaly
	case models.EventDeploymentStuck:
		return models.AlertTypeDeployStuck
	}
	return ""
}

// alertTitle returns the human title for an alert type. Pure function.
func alertTitle(alertType, envName string) string {
	titles := map[string]string{
		models.AlertTypeServiceDown:   "Service down",
		models.AlertTypeTasksDegraded: "Tasks degraded",
		models.AlertTypeHighErrorRate: "High error rate",
		models.AlertTypeHighLatency:   "High latency",
		models.AlertTypeCrashLoop:     "Crash loop detected",
		models.AlertTypeLogAnomaly:    "Log anomaly detected",
		models.AlertTypeDeployStuck:   "Deployment stuck",
	}
	t := titles[alertType]
	if t == "" {
		t = alertType
	}
	if envName != "" {
		t += " — " + envName
	}
	return t
}

// EvaluateEvent decides whether an operational event becomes an alert. Called
// asynchronously from events.Service after every event is persisted. Recovery
// events resolve alerts instead of creating them.
func (a *AlertEngine) EvaluateEvent(ctx context.Context, ev models.OperationalEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("alert engine: panic recovered", "component", "monitor.alerts", "panic", r)
		}
	}()

	// Recovery paths first.
	switch ev.EventType {
	case models.EventRuntimeServiceRecovered:
		if ev.EnvironmentID != nil {
			a.resolveAlerts(ctx, ev.ProjectID, *ev.EnvironmentID,
				models.AlertTypeServiceDown, models.AlertTypeTasksDegraded,
				models.AlertTypeHighErrorRate, models.AlertTypeHighLatency)
		}
		return
	case models.EventECSStable:
		if ev.EnvironmentID != nil {
			a.resolveAlerts(ctx, ev.ProjectID, *ev.EnvironmentID, models.AlertTypeDeployStuck)
		}
		return
	}

	alertType := MapEventToAlert(ev.EventType, ev.Payload)
	if alertType == "" {
		return
	}

	envID := uuid.Nil
	if ev.EnvironmentID != nil {
		envID = *ev.EnvironmentID
	}

	// Dedup: an open alert of the same type for this environment within the window.
	var exists bool
	err := a.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM alerts
			WHERE project_id = $1
			  AND COALESCE(environment_id, '00000000-0000-0000-0000-000000000000'::uuid) = $2
			  AND alert_type = $3
			  AND status = $4
			  AND triggered_at > NOW() - $5::interval
		)`, ev.ProjectID, envID, alertType, models.AlertStatusOpen, alertDedupWindow.String(),
	).Scan(&exists)
	if err != nil || exists {
		return
	}

	// Snooze preferences.
	if a.isSnoozed(ctx, ev.ProjectID, ev.EnvironmentID, alertType) {
		return
	}

	// Context for the title/summary.
	envName, projectName := a.names(ctx, ev.ProjectID, ev.EnvironmentID)
	title := alertTitle(alertType, envName)
	summary := a.generateSummary(ctx, ev, envName, projectName, title)

	alert := models.Alert{
		ProjectID: ev.ProjectID,
		AlertType: alertType,
		Severity:  ev.Severity,
		Title:     title,
		Summary:   summary,
		Status:    models.AlertStatusOpen,
	}
	alert.EnvironmentID = ev.EnvironmentID

	sourceIDs := []uuid.UUID{}
	if ev.ID != uuid.Nil {
		sourceIDs = append(sourceIDs, ev.ID)
	}

	err = a.db.Pool.QueryRow(ctx, `
		INSERT INTO alerts (project_id, org_id, environment_id, alert_type, severity, title, summary, source_event_ids)
		VALUES ($1, (SELECT org_id FROM projects WHERE id = $1), $2, $3, $4, $5, $6, $7)
		RETURNING id, triggered_at, created_at`,
		alert.ProjectID, alert.EnvironmentID, alert.AlertType, alert.Severity,
		alert.Title, alert.Summary, sourceIDs,
	).Scan(&alert.ID, &alert.TriggeredAt, &alert.CreatedAt)
	if err != nil {
		slog.Error("alert engine: failed to insert alert", "component", "monitor.alerts",
			"project_id", ev.ProjectID, "error", err)
		return
	}

	if b, err := json.Marshal(alert); err == nil {
		a.hub.Broadcast(alert.ProjectID.String(), ws.Message{Type: "alert", Payload: string(b)})
	}

	a.notifyOwner(ctx, alert, projectName, envName)
}

// ResolveAlert resolves open alerts of the given types for an environment.
// Called on recovery events and successful deploys.
func (a *AlertEngine) ResolveAlert(ctx context.Context, projectID, environmentID uuid.UUID, alertTypes ...string) {
	a.resolveAlerts(ctx, projectID, environmentID, alertTypes...)
}

func (a *AlertEngine) resolveAlerts(ctx context.Context, projectID, environmentID uuid.UUID, alertTypes ...string) {
	if len(alertTypes) == 0 {
		return
	}
	rows, err := a.db.Pool.Query(ctx, `
		UPDATE alerts SET status = $1, resolved_at = NOW()
		WHERE project_id = $2 AND environment_id = $3
		  AND alert_type = ANY($4) AND status = $5
		RETURNING id, alert_type`,
		models.AlertStatusResolved, projectID, environmentID, alertTypes, models.AlertStatusOpen,
	)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var alertType string
		if rows.Scan(&id, &alertType) == nil {
			payload, _ := json.Marshal(map[string]string{"id": id.String(), "alert_type": alertType})
			a.hub.Broadcast(projectID.String(), ws.Message{Type: "alert_resolved", Payload: string(payload)})
		}
	}
}

// isSnoozed checks alert_preferences for an active (non-expired) snooze.
func (a *AlertEngine) isSnoozed(ctx context.Context, projectID uuid.UUID, envID *uuid.UUID, alertType string) bool {
	var snoozed bool
	err := a.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM alert_preferences
			WHERE project_id = $1
			  AND (environment_id = $2 OR ($2::uuid IS NULL AND environment_id IS NULL) OR environment_id IS NULL)
			  AND alert_type = $3
			  AND snoozed_until > NOW()
		)`, projectID, envID, alertType,
	).Scan(&snoozed)
	return err == nil && snoozed
}

// names loads display names for the summary prompt.
func (a *AlertEngine) names(ctx context.Context, projectID uuid.UUID, envID *uuid.UUID) (envName, projectName string) {
	a.db.Pool.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, projectID).Scan(&projectName)
	if envID != nil {
		a.db.Pool.QueryRow(ctx, `SELECT name FROM environments WHERE id = $1`, envID).Scan(&envName)
	}
	return envName, projectName
}

// generateSummary asks the LLM for a one-sentence description, falling back to
// a deterministic summary when the LLM is unavailable.
func (a *AlertEngine) generateSummary(ctx context.Context, ev models.OperationalEvent, envName, projectName, title string) string {
	payloadJSON, _ := json.Marshal(ev.Payload)
	prompt := fmt.Sprintf(
		"In one sentence, describe this infrastructure alert for a developer. "+
			"Be specific about what is wrong and what service is affected. "+
			"Event: %s on environment %s, project %s. Payload: %s. Max 120 characters. "+
			"Respond with the sentence only.",
		ev.EventType, envName, projectName, string(payloadJSON),
	)

	llmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	summary, err := a.llm.Complete(llmCtx, "", prompt, 100)
	if err != nil || strings.TrimSpace(summary) == "" {
		return fmt.Sprintf("%s in %s/%s", title, projectName, envName)
	}
	summary = strings.TrimSpace(summary)
	if len(summary) > 200 {
		summary = summary[:200]
	}
	return summary
}

// notifyOwner emails the project owner if their notification preferences allow it.
func (a *AlertEngine) notifyOwner(ctx context.Context, alert models.Alert, projectName, envName string) {
	var email string
	var enabled bool
	err := a.db.Pool.QueryRow(ctx, `
		SELECT u.email, u.notifications_enabled AND u.notify_alert_fired
		FROM users u JOIN projects p ON p.user_id = u.id
		WHERE p.id = $1`, alert.ProjectID,
	).Scan(&email, &enabled)
	if err != nil || !enabled {
		return
	}

	alertURL := strings.TrimRight(os.Getenv("FRONTEND_URL"), "/") + "/projects/" + alert.ProjectID.String()
	if err := a.emailSvc.SendAlert(ctx, email, projectName, envName, alert.Title, alert.Summary, alertURL); err != nil {
		slog.Warn("alert engine: email failed", "component", "monitor.alerts",
			"project_id", alert.ProjectID, "error", err)
	}
}
