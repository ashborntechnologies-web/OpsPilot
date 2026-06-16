// Package postmortem generates structured, editable, exportable postmortems from a
// resolved incident's data (timeline, root cause, AI actions, the triggering deploy, and
// project memory). Generation is async (an Asynq job enqueued on incident resolve — see
// ADR-014) so resolving an incident never blocks on Claude. The full set of published
// postmortems doubles as a compliance/SOC2 incident record.
package postmortem

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/internal/memory"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

type Service struct {
	db     *models.DB
	llm    *llm.Client
	memory *memory.Service
}

func NewService(db *models.DB, llmClient *llm.Client, memorySvc *memory.Service) *Service {
	return &Service{db: db, llm: llmClient, memory: memorySvc}
}

// GeneratePostmortem builds a structured postmortem for a resolved incident, stores it as
// a draft, mirrors the markdown to incidents.postmortem (backward compat), and records a
// successful_fix memory for future diagnoses.
func (s *Service) GeneratePostmortem(ctx context.Context, incidentID uuid.UUID) (*models.Postmortem, error) {
	// 1. Incident core.
	var (
		orgID, projectID      uuid.UUID
		title, severity       string
		rootCause, resolution *string
		confidence            *float64
		createdAt             time.Time
		resolvedAt            *time.Time
		deploymentID          *uuid.UUID
	)
	err := s.db.Pool.QueryRow(ctx, `
		SELECT org_id, project_id, COALESCE(title,''), severity, root_cause, resolution,
		       confidence_score, created_at, resolved_at, deployment_id
		FROM incidents WHERE id = $1`, incidentID,
	).Scan(&orgID, &projectID, &title, &severity, &rootCause, &resolution,
		&confidence, &createdAt, &resolvedAt, &deploymentID)
	if err != nil {
		return nil, fmt.Errorf("postmortem: load incident: %w", err)
	}
	duration := "unknown"
	if resolvedAt != nil {
		duration = resolvedAt.Sub(createdAt).Round(time.Minute).String()
	}

	// 2-5. Supporting context.
	timeline := s.renderTimeline(ctx, incidentID)
	actions := s.renderActions(ctx, incidentID)
	deployInfo := s.renderDeployment(ctx, deploymentID)
	memoryCtx := s.renderMemory(ctx, projectID)

	// 6 + 7. Context → Claude.
	md := s.generateMarkdown(ctx, title, severity, duration, deref(rootCause), deref(resolution), confidence, timeline, actions, deployInfo, memoryCtx)

	// Parse action items from the rendered markdown.
	items := parseActionItems(md)
	itemsJSON, _ := json.Marshal(items)

	pmTitle := title
	if pmTitle == "" {
		pmTitle = "Incident postmortem"
	}

	// 8 + 9. Upsert the postmortem (draft) keyed by incident.
	var pm models.Postmortem
	err = s.db.Pool.QueryRow(ctx, `
		INSERT INTO postmortems (incident_id, org_id, project_id, title, status, content_markdown, action_items, generated_at)
		VALUES ($1, $2, $3, $4, 'draft', $5, $6, NOW())
		ON CONFLICT (incident_id) DO UPDATE SET
		    content_markdown = EXCLUDED.content_markdown,
		    action_items = EXCLUDED.action_items,
		    title = EXCLUDED.title,
		    generated_at = NOW(),
		    updated_at = NOW()
		RETURNING id, status, generated_at, created_at, updated_at`,
		incidentID, orgID, projectID, pmTitle, md, itemsJSON,
	).Scan(&pm.ID, &pm.Status, &pm.GeneratedAt, &pm.CreatedAt, &pm.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("postmortem: store: %w", err)
	}
	pm.IncidentID, pm.OrgID, pm.ProjectID, pm.Title = incidentID, orgID, projectID, pmTitle
	pm.ContentMarkdown, pm.ActionItems = md, items

	// 9b. Mirror to incidents.postmortem for backward compatibility.
	_, _ = s.db.Pool.Exec(ctx, `UPDATE incidents SET postmortem = $1 WHERE id = $2`, md, incidentID)

	// 10. Record the fix in project memory for future diagnosis context.
	if s.memory != nil && (rootCause != nil || resolution != nil) {
		fix := fmt.Sprintf("Resolved incident %q. Root cause: %s. Fix: %s",
			pmTitle, firstLine(deref(rootCause)), firstLine(deref(resolution)))
		s.memory.RecordSuccessfulFix(ctx, projectID, fix)
	}

	slog.Info("postmortem generated", "component", "postmortem", "incident", incidentID, "action_items", len(items))
	return &pm, nil
}

// generateMarkdown asks Claude for the structured postmortem, with a template fallback so
// generation never hard-fails on an AI outage.
func (s *Service) generateMarkdown(ctx context.Context, title, severity, duration, rootCause, resolution string, confidence *float64, timeline, actions, deployInfo, memoryCtx string) string {
	system := "You are an SRE writing a concise, blameless postmortem. Output GitHub-flavored " +
		"markdown with EXACTLY these level-2 sections in order: " +
		"## Summary (2-3 sentences: what happened and the impact), " +
		"## Timeline (bullet points of key events with times), " +
		"## Root Cause (plain English, from the diagnosis), " +
		"## Contributing Factors (list of what made this worse or more likely), " +
		"## What Went Well (at least ONE item — always find one), " +
		"## Action Items (numbered list; each item formatted EXACTLY as " +
		"`[the action] Owner: [ROLE] Priority: [HIGH/MED/LOW]`). " +
		"Be specific and factual; do not invent details not present in the input."

	conf := ""
	if confidence != nil {
		conf = fmt.Sprintf("\nDiagnosis confidence: %.0f%%", *confidence*100)
	}
	user := fmt.Sprintf(
		"Incident: %s\nSeverity: %s\nDuration: %s%s\n\nRoot cause:\n%s\n\nResolution:\n%s\n\n"+
			"Triggering deploy:\n%s\n\nTimeline:\n%s\n\nActions taken:\n%s\n\nProject history:\n%s",
		title, severity, duration, conf, orDefault(rootCause), orDefault(resolution),
		deployInfo, timeline, actions, memoryCtx)

	md, err := s.llm.Complete(ctx, system, user, 1500)
	if err != nil || strings.TrimSpace(md) == "" {
		return fmt.Sprintf("## Summary\n%s (%s, duration %s).\n\n## Timeline\n%s\n\n## Root Cause\n%s\n\n"+
			"## Contributing Factors\n- _To be completed._\n\n## What Went Well\n- The incident was detected and resolved.\n\n"+
			"## Action Items\n1. Review and complete this postmortem. Owner: [SRE] Priority: [MED]",
			title, severity, duration, timeline, orDefault(rootCause))
	}
	return md
}

// parseActionItems extracts the numbered items under "## Action Items" into structured
// records. Owner/Priority are best-effort (the model is told to format them).
func parseActionItems(md string) []models.ActionItem {
	items := []models.ActionItem{}
	lines := strings.Split(md, "\n")
	inSection := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			inSection = strings.Contains(strings.ToLower(t), "action item")
			continue
		}
		if !inSection || t == "" {
			continue
		}
		// Split out Owner:/Priority: tags, leaving the action text.
		item := t
		owner, priority := "", ""
		if i := regexpIndexFold(item, "owner:"); i >= 0 {
			rest := item[i+len("owner:"):]
			item = strings.TrimSpace(item[:i])
			// rest may contain "X Priority: Y"
			if j := regexpIndexFold(rest, "priority:"); j >= 0 {
				owner = cleanBracket(rest[:j])
				priority = normalizePriority(cleanBracket(rest[j+len("priority:"):]))
			} else {
				owner = cleanBracket(rest)
			}
		} else if i := regexpIndexFold(item, "priority:"); i >= 0 {
			priority = normalizePriority(cleanBracket(item[i+len("priority:"):]))
			item = strings.TrimSpace(item[:i])
		}
		item = strings.TrimLeft(item, "0123456789.)-* \t")
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		items = append(items, models.ActionItem{Item: item, Owner: owner, Priority: priority, Status: "open"})
	}
	return items
}

func regexpIndexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

func cleanBracket(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "[]")
	return strings.TrimSpace(s)
}

func normalizePriority(p string) string {
	switch strings.ToUpper(strings.TrimSpace(p)) {
	case "HIGH":
		return "HIGH"
	case "LOW":
		return "LOW"
	case "MED", "MEDIUM":
		return "MED"
	}
	return ""
}

// ─── context renderers ────────────────────────────────────────────────────────

func (s *Service) renderTimeline(ctx context.Context, incidentID uuid.UUID) string {
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
		if len(content) > 500 {
			content = content[:500] + "…"
		}
		fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", ts.UTC().Format("15:04"), author, entryType, content)
	}
	if b.Len() == 0 {
		return "n/a"
	}
	return b.String()
}

func (s *Service) renderActions(ctx context.Context, incidentID uuid.UUID) string {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT action_type, status, COALESCE(result->>'message', result->>'error', '')
		FROM ai_actions WHERE incident_id = $1 ORDER BY proposed_at ASC`, incidentID)
	if err != nil {
		return "none"
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var at, st, detail string
		if rows.Scan(&at, &st, &detail) == nil {
			fmt.Fprintf(&b, "- %s (%s) %s\n", at, st, detail)
		}
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}

func (s *Service) renderDeployment(ctx context.Context, deploymentID *uuid.UUID) string {
	if deploymentID == nil {
		return "Not deploy-triggered (runtime anomaly)."
	}
	var sha string
	var msg *string
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT commit_sha, commit_message FROM deployments WHERE id = $1`, *deploymentID,
	).Scan(&sha, &msg); err != nil {
		return "n/a"
	}
	short := sha
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("commit %s — %s", short, deref(msg))
}

func (s *Service) renderMemory(ctx context.Context, projectID uuid.UUID) string {
	if s.memory == nil {
		return "n/a"
	}
	mems, err := s.memory.GetRelevantMemory(ctx, projectID, 3)
	if err != nil || len(mems) == 0 {
		return "n/a"
	}
	return memory.FormatForPrompt(mems)
}

// ─── small helpers ────────────────────────────────────────────────────────────

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orDefault(s string) string {
	if strings.TrimSpace(s) == "" {
		return "n/a"
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		return s[:i]
	}
	if len(s) > 160 {
		return s[:160]
	}
	return s
}
