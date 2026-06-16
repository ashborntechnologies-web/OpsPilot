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
	slack    SlackAlertNotifier
}

// SlackAlertNotifier posts alerts to an org's Slack channel. Implemented by
// *slack.Service; injected to avoid a monitor↔slack import cycle. Best-effort.
type SlackAlertNotifier interface {
	PostAlert(ctx context.Context, orgID uuid.UUID, alert models.Alert, projectName, envName, incidentURL string) error
}

func NewAlertEngine(db *models.DB, llmClient *llm.Client, hub *ws.Hub, emailSvc *notify.EmailService) *AlertEngine {
	return &AlertEngine{db: db, llm: llmClient, hub: hub, emailSvc: emailSvc}
}

// SetSlackNotifier wires Slack alert notifications (optional).
func (a *AlertEngine) SetSlackNotifier(n SlackAlertNotifier) { a.slack = n }

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
	summary, evidenceText := a.generateSummary(ctx, ev, envName, projectName, title)

	alert := models.Alert{
		ProjectID:    ev.ProjectID,
		AlertType:    alertType,
		Severity:     ev.Severity,
		Title:        title,
		Summary:      summary,
		EvidenceText: &evidenceText,
		Status:       models.AlertStatusOpen,
	}
	alert.EnvironmentID = ev.EnvironmentID

	sourceIDs := []uuid.UUID{}
	if ev.ID != uuid.Nil {
		sourceIDs = append(sourceIDs, ev.ID)
	}

	err = a.db.Pool.QueryRow(ctx, `
		INSERT INTO alerts (project_id, org_id, environment_id, alert_type, severity, title, summary, evidence_text, source_event_ids)
		VALUES ($1, (SELECT org_id FROM projects WHERE id = $1), $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, triggered_at, created_at`,
		alert.ProjectID, alert.EnvironmentID, alert.AlertType, alert.Severity,
		alert.Title, alert.Summary, evidenceText, sourceIDs,
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
// generateSummary returns an LLM one-sentence summary plus a deterministic
// evidence_text (1–2 sentences) explaining what in the event payload triggered the alert
// — the alert's explainability, derived from real data rather than the model.
func (a *AlertEngine) generateSummary(ctx context.Context, ev models.OperationalEvent, envName, projectName, title string) (summary, evidenceText string) {
	evidenceText = alertEvidence(ev)

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
		return fmt.Sprintf("%s in %s/%s", title, projectName, envName), evidenceText
	}
	summary = strings.TrimSpace(summary)
	if len(summary) > 200 {
		summary = summary[:200]
	}
	return summary, evidenceText
}

// alertEvidence builds a short, factual explanation of what triggered an alert from the
// operational event payload (no LLM) — e.g. error rate, task counts, matched log pattern.
func alertEvidence(ev models.OperationalEvent) string {
	p := ev.Payload
	num := func(k string) (float64, bool) {
		if v, ok := p[k]; ok {
			switch n := v.(type) {
			case float64:
				return n, true
			case int:
				return float64(n), true
			case int32:
				return float64(n), true
			}
		}
		return 0, false
	}
	switch ev.EventType {
	case models.EventRuntimeServiceDown:
		return "Triggered because the service has 0 running tasks while the desired count is above zero."
	case models.EventRuntimeTasksDegraded:
		r, ok1 := num("running")
		d, ok2 := num("desired")
		if ok1 && ok2 {
			return fmt.Sprintf("Triggered because only %.0f of %.0f desired tasks are running.", r, d)
		}
		return "Triggered because fewer tasks are running than desired."
	case models.EventRuntimeHighErrorRate:
		if pct, ok := num("error_rate_pct"); ok {
			return fmt.Sprintf("Triggered by an elevated 5xx error rate of %.1f%% over the sampling window.", pct)
		}
		return "Triggered by an elevated 5xx error rate from the load balancer."
	case models.EventRuntimeHighLatency:
		if ms, ok := num("p99_latency_ms"); ok {
			return fmt.Sprintf("Triggered by p99 latency of %.0f ms exceeding the threshold.", ms)
		}
		return "Triggered by p99 latency exceeding the threshold."
	case models.EventRuntimeLogAnomaly:
		pt, _ := p["pattern_type"].(string)
		if n, ok := num("line_count"); ok && pt != "" {
			return fmt.Sprintf("Matched the %q anomaly pattern in %.0f recent log line(s).", pt, n)
		}
		if pt != "" {
			return fmt.Sprintf("Matched the %q anomaly pattern in recent application logs.", pt)
		}
		return "Matched an anomaly pattern in recent application logs."
	default:
		return fmt.Sprintf("Triggered by a %s event from continuous monitoring.", strings.ReplaceAll(ev.EventType, ".", " "))
	}
}

// notifyOwner emails the project owner (if their preferences allow) and posts to the
// org's Slack alert channel (if connected). Both are best-effort.
func (a *AlertEngine) notifyOwner(ctx context.Context, alert models.Alert, projectName, envName string) {
	// Link to the incident war room. If an incident is already open for this
	// project/environment (e.g. a recurring issue, or the auto-diagnosis from an earlier
	// alert in the same window already opened one), deep-link straight to that war room;
	// otherwise fall back to the incident list (the diagnosis job that follows this alert
	// opens the specific incident moments later, where it surfaces open-first).
	frontend := strings.TrimRight(os.Getenv("FRONTEND_URL"), "/")
	alertURL := frontend + "/incidents"
	var openIncidentID uuid.UUID
	if err := a.db.Pool.QueryRow(ctx, `
		SELECT id FROM incidents
		WHERE project_id = $1 AND status <> 'resolved'
		  AND ($2::uuid IS NULL OR environment_id = $2)
		ORDER BY created_at DESC LIMIT 1`,
		alert.ProjectID, alert.EnvironmentID).Scan(&openIncidentID); err == nil {
		alertURL = frontend + "/incidents/" + openIncidentID.String()
	}

	var email string
	var enabled bool
	var orgID *uuid.UUID
	err := a.db.Pool.QueryRow(ctx, `
		SELECT u.email, u.notifications_enabled AND u.notify_alert_fired, p.org_id
		FROM users u JOIN projects p ON p.user_id = u.id
		WHERE p.id = $1`, alert.ProjectID,
	).Scan(&email, &enabled, &orgID)
	if err != nil {
		return
	}

	// On-call quiet hours (ADR-016): during quiet hours, warn-severity alerts are
	// suppressed from email + Slack (the DB row + WS broadcast already happened, so an
	// engineer at their desk still sees it). Error-severity alerts always break through —
	// critical issues wake people up — with a note on the Slack message.
	quiet := false
	if orgID != nil {
		quiet, _ = a.CheckQuietHours(ctx, *orgID)
	}
	if quiet && alert.Severity != models.SeverityError {
		slog.Info(fmt.Sprintf("alert suppressed during quiet hours: %s", alert.ID),
			"component", "monitor.alerts", "alert_id", alert.ID, "org_id", orgID, "severity", alert.Severity)
		return
	}

	if enabled {
		if err := a.emailSvc.SendAlert(ctx, email, projectName, envName, alert.Title, alert.Summary, alertURL); err != nil {
			slog.Warn("alert engine: email failed", "component", "monitor.alerts",
				"project_id", alert.ProjectID, "error", err)
		}
	}

	// Slack — independent of email preferences (channel-level opt-in via connecting Slack).
	if a.slack != nil && orgID != nil {
		slackAlert := alert
		if quiet {
			// Reaching here with quiet == true means error severity broke through.
			slackAlert.Summary = strings.TrimSpace(alert.Summary + "\n⚠️ Sent outside quiet hours due to error severity")
		}
		if err := a.slack.PostAlert(ctx, *orgID, slackAlert, projectName, envName, alertURL); err != nil {
			slog.Warn("alert engine: slack notify failed", "component", "monitor.alerts",
				"project_id", alert.ProjectID, "error", err)
		}
	}
}

// CheckQuietHours reports whether the org is currently within its configured on-call quiet
// window (quiet hours or a quiet day, evaluated in the org's timezone). Returns false when no
// schedule is configured or on any error — failing open so alerts are never silently lost.
func (a *AlertEngine) CheckQuietHours(ctx context.Context, orgID uuid.UUID) (bool, error) {
	var tz, startStr, endStr string
	var quietDays []string
	err := a.db.Pool.QueryRow(ctx, `
		SELECT timezone, to_char(quiet_hours_start, 'HH24:MI'), to_char(quiet_hours_end, 'HH24:MI'), quiet_days
		FROM oncall_schedules WHERE org_id = $1`, orgID).Scan(&tz, &startStr, &endStr, &quietDays)
	if err != nil {
		return false, err // no schedule (or error) → not quiet
	}

	loc, lerr := time.LoadLocation(tz)
	if lerr != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)

	// Quiet day? (weekday names are stored lowercase, e.g. "saturday")
	today := strings.ToLower(now.Weekday().String())
	for _, d := range quietDays {
		if strings.ToLower(strings.TrimSpace(d)) == today {
			return true, nil
		}
	}

	// Quiet hours? Supports overnight windows (start > end, e.g. 22:00–08:00).
	start, ok1 := parseHHMM(startStr)
	end, ok2 := parseHHMM(endStr)
	if !ok1 || !ok2 || start == end {
		return false, nil // unset/degenerate window → only quiet days apply
	}
	nowMin := now.Hour()*60 + now.Minute()
	if start < end {
		return nowMin >= start && nowMin < end, nil
	}
	// Overnight window wraps past midnight.
	return nowMin >= start || nowMin < end, nil
}

// parseHHMM parses "HH:MM" into minutes-since-midnight.
func parseHHMM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}
