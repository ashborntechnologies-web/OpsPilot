// Package incidents implements the incident war room: incidents are first-class,
// lifecycle-tracked objects (open → investigating → resolved) with a shared real-time
// timeline (AI + human entries) and an approval flow for proposed remediation actions.
// On resolution the AI generates a markdown postmortem. Diagnosis results feed in via
// CreateIncident; the war-room WebSocket (pkg/ws incident rooms) streams the timeline.
package incidents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Service struct {
	db  *models.DB
	llm *llm.Client
	hub *ws.Hub
}

func NewService(db *models.DB, llmClient *llm.Client, hub *ws.Hub) *Service {
	return &Service{db: db, llm: llmClient, hub: hub}
}

// CreateParams describes a new (or re-diagnosed) incident, produced by the diagnosis
// pipeline when a deploy fails or a runtime anomaly is detected.
type CreateParams struct {
	ProjectID         uuid.UUID
	DeploymentID      *uuid.UUID
	EnvironmentID     *uuid.UUID
	Trigger           string // deploy_failure | runtime_anomaly
	Severity          string // warn | error
	Title             string // optional; derived from the root cause when empty
	RootCause         string
	Resolution        string
	RawLogs           string
	DiagnosisMarkdown string // full diagnosis text → first timeline entry
}

// CreateIncident creates an incident and posts the AI diagnosis as its first timeline
// entry. It deduplicates: if a non-resolved incident already exists for the same
// deployment (or the same environment for a runtime anomaly), the diagnosis is appended
// to that incident's timeline instead of opening a duplicate. Returns the incident ID.
func (s *Service) CreateIncident(ctx context.Context, p CreateParams) (uuid.UUID, error) {
	if existing, found := s.findOpenIncident(ctx, p); found {
		if strings.TrimSpace(p.DiagnosisMarkdown) != "" {
			_, _ = s.postTimelineEntry(ctx, existing, models.IncidentAuthorAI, nil,
				p.DiagnosisMarkdown, models.IncidentEntryDiagnosis, map[string]any{"rediagnosis": true})
		}
		return existing, nil
	}

	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = deriveTitle(p.RootCause, p.Trigger)
	}
	severity := p.Severity
	if severity == "" {
		severity = models.SeverityWarn
	}

	var id uuid.UUID
	err := s.db.Pool.QueryRow(ctx, `
		INSERT INTO incidents
		    (project_id, org_id, deployment_id, environment_id, trigger,
		     root_cause, resolution, raw_logs, title, status, severity)
		VALUES ($1, (SELECT org_id FROM projects WHERE id = $1), $2, $3, $4,
		        $5, $6, $7, $8, 'open', $9)
		RETURNING id`,
		p.ProjectID, p.DeploymentID, p.EnvironmentID, p.Trigger,
		p.RootCause, p.Resolution, p.RawLogs, title, severity,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create incident: %w", err)
	}

	// First timeline entry — the AI diagnosis.
	content := strings.TrimSpace(p.DiagnosisMarkdown)
	if content == "" {
		content = p.RootCause
	}
	_, _ = s.postTimelineEntry(ctx, id, models.IncidentAuthorAI, nil, content, models.IncidentEntryDiagnosis, nil)

	// If the diagnosis suggested a fix, surface it as a pending AI-proposed action so
	// the war room's actions panel has something actionable.
	if fix := strings.TrimSpace(p.Resolution); fix != "" {
		_, _ = s.db.Pool.Exec(ctx, `
			INSERT INTO incident_actions (incident_id, proposed_by, action_type, parameters, status)
			VALUES ($1, 'ai', 'suggested_fix', $2, 'pending')`,
			id, map[string]any{"description": fix})
	}

	slog.Info("incident opened", "component", "incidents", "incident", id, "trigger", p.Trigger)
	return id, nil
}

// findOpenIncident returns a non-resolved incident matching the diagnosis target, for
// deduplication.
func (s *Service) findOpenIncident(ctx context.Context, p CreateParams) (uuid.UUID, bool) {
	var id uuid.UUID
	switch {
	case p.DeploymentID != nil:
		err := s.db.Pool.QueryRow(ctx,
			`SELECT id FROM incidents WHERE deployment_id = $1 AND status <> 'resolved'
			 ORDER BY created_at DESC LIMIT 1`, *p.DeploymentID).Scan(&id)
		return id, err == nil
	case p.EnvironmentID != nil:
		err := s.db.Pool.QueryRow(ctx,
			`SELECT id FROM incidents WHERE environment_id = $1 AND trigger = $2 AND status <> 'resolved'
			 ORDER BY created_at DESC LIMIT 1`, *p.EnvironmentID, p.Trigger).Scan(&id)
		return id, err == nil
	}
	return uuid.Nil, false
}

// postTimelineEntry inserts a timeline entry and broadcasts it to the war room.
func (s *Service) postTimelineEntry(ctx context.Context, incidentID uuid.UUID, authorType string, authorID *uuid.UUID, content, entryType string, metadata map[string]any) (models.IncidentTimelineEntry, error) {
	if metadata == nil {
		metadata = map[string]any{}
	}
	var e models.IncidentTimelineEntry
	err := s.db.Pool.QueryRow(ctx, `
		INSERT INTO incident_timeline (incident_id, author_type, author_id, content, entry_type, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, incident_id, author_type, author_id, content, entry_type, created_at`,
		incidentID, authorType, authorID, content, entryType, metadata,
	).Scan(&e.ID, &e.IncidentID, &e.AuthorType, &e.AuthorID, &e.Content, &e.EntryType, &e.CreatedAt)
	if err != nil {
		return e, err
	}
	e.Metadata = metadata
	if authorID != nil {
		_ = s.db.Pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, *authorID).Scan(&e.AuthorName)
	}
	s.broadcast(incidentID, "incident_timeline", e)
	return e, nil
}

// broadcast JSON-encodes a payload and sends it to all war-room subscribers.
func (s *Service) broadcast(incidentID uuid.UUID, msgType string, payload any) {
	if s.hub == nil {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.hub.Broadcast(incidentID.String(), ws.Message{Type: msgType, Payload: string(b)})
}

// GeneratePostmortem asks Claude to write a markdown postmortem from the incident's full
// timeline, root cause, actions taken, and duration, stores it, and returns it.
func (s *Service) GeneratePostmortem(ctx context.Context, incidentID uuid.UUID) (string, error) {
	var (
		title, severity, trigger string
		rootCause, resolution    *string
		createdAt                time.Time
		resolvedAt               *time.Time
	)
	err := s.db.Pool.QueryRow(ctx, `
		SELECT COALESCE(title,''), severity, trigger, root_cause, resolution, created_at, resolved_at
		FROM incidents WHERE id = $1`, incidentID,
	).Scan(&title, &severity, &trigger, &rootCause, &resolution, &createdAt, &resolvedAt)
	if err != nil {
		return "", err
	}

	timeline := s.renderTimelineText(ctx, incidentID)
	actions := s.renderActionsText(ctx, incidentID)
	duration := "unknown"
	if resolvedAt != nil {
		duration = resolvedAt.Sub(createdAt).Round(time.Minute).String()
	}

	system := "You are an SRE writing a concise, blameless postmortem. Output GitHub-" +
		"flavored markdown with exactly these sections as level-2 headings: " +
		"## Summary, ## Timeline, ## Root Cause, ## Contributing Factors, ## Action Items. " +
		"Be specific and factual; do not invent details not present in the input. Keep it tight."

	user := fmt.Sprintf(
		"Incident: %s\nSeverity: %s\nTrigger: %s\nDuration: %s\n\nRoot cause:\n%s\n\nResolution:\n%s\n\nTimeline:\n%s\n\nActions:\n%s",
		title, severity, trigger, duration, derefOr(rootCause, "n/a"), derefOr(resolution, "n/a"), timeline, actions)

	md, err := s.llm.Complete(ctx, system, user, 1200)
	if err != nil {
		// Fall back to a minimal template so resolution is never blocked by an AI outage.
		md = fmt.Sprintf("## Summary\n%s (%s, %s).\n\n## Timeline\n%s\n\n## Root Cause\n%s\n\n## Contributing Factors\n_To be filled in._\n\n## Action Items\n- [ ] Review and complete this postmortem.",
			title, severity, duration, timeline, derefOr(rootCause, "n/a"))
	}

	if _, err := s.db.Pool.Exec(ctx, `UPDATE incidents SET postmortem = $1 WHERE id = $2`, md, incidentID); err != nil {
		return md, err
	}
	return md, nil
}

// renderTimelineText renders the timeline as plain text for the postmortem prompt.
func (s *Service) renderTimelineText(ctx context.Context, incidentID uuid.UUID) string {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT t.author_type, COALESCE(u.email,'AI'), t.entry_type, t.content, t.created_at
		FROM incident_timeline t LEFT JOIN users u ON u.id = t.author_id
		WHERE t.incident_id = $1 ORDER BY t.created_at ASC`, incidentID)
	if err != nil {
		return "n/a"
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var authorType, author, entryType, content string
		var ts time.Time
		if rows.Scan(&authorType, &author, &entryType, &content, &ts) != nil {
			continue
		}
		if len(content) > 600 {
			content = content[:600] + "…"
		}
		fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", ts.UTC().Format("15:04"), author, entryType, content)
	}
	if b.Len() == 0 {
		return "n/a"
	}
	return b.String()
}

func (s *Service) renderActionsText(ctx context.Context, incidentID uuid.UUID) string {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT proposed_by, action_type, status FROM incident_actions WHERE incident_id = $1 ORDER BY created_at ASC`, incidentID)
	if err != nil {
		return "n/a"
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var by, at, st string
		if rows.Scan(&by, &at, &st) == nil {
			fmt.Fprintf(&b, "- %s proposed %s (%s)\n", by, at, st)
		}
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}

// deriveTitle builds a short incident title from the root cause (first sentence) or the
// trigger when no root cause is available.
func deriveTitle(rootCause, trigger string) string {
	rc := strings.TrimSpace(rootCause)
	if rc != "" {
		if i := strings.IndexAny(rc, ".\n"); i > 0 {
			rc = rc[:i]
		}
		if len(rc) > 100 {
			rc = rc[:100]
		}
		return rc
	}
	switch trigger {
	case models.IncidentTriggerDeployFailure:
		return "Deployment failure"
	case models.IncidentTriggerRuntimeAnomaly:
		return "Runtime anomaly"
	default:
		return "Incident"
	}
}

func derefOr(s *string, fallback string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return fallback
	}
	return *s
}

// loadIncidentOrg returns the org that owns an incident (for access checks).
func (s *Service) loadIncidentOrg(ctx context.Context, incidentID uuid.UUID) (uuid.UUID, error) {
	var orgID *uuid.UUID
	err := s.db.Pool.QueryRow(ctx, `SELECT org_id FROM incidents WHERE id = $1`, incidentID).Scan(&orgID)
	if err == pgx.ErrNoRows || (err == nil && orgID == nil) {
		return uuid.UUID{}, models.ErrNoMembership
	}
	if err != nil {
		return uuid.UUID{}, err
	}
	return *orgID, nil
}
