// Package summary generates the daily operational briefing — an AI-written morning
// summary of an org's last 24h (deploys, incidents + MTTR, alerts, recurring failures)
// with up to three data-grounded recommendations. Summaries are stored, posted to Slack,
// and emailed to admins/engineers. An hourly scheduler tick enqueues generation for orgs
// whose configured delivery time falls in the current hour (in their timezone).
package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	awssvc "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/internal/notify"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

// SlackPoster posts a markdown summary to an org's Slack summary channel and reports
// whether it was delivered. Satisfied by *slack.Service (injected to avoid a cycle).
type SlackPoster interface {
	PostDailySummary(ctx context.Context, orgID uuid.UUID, markdown string) (bool, error)
}

// SummaryEnqueuer enqueues a per-org generation job (satisfied by *queue.Client).
type SummaryEnqueuer interface {
	EnqueueGenerateSummary(orgID string) error
}

type Service struct {
	db          *models.DB
	llm         *llm.Client
	email       *notify.EmailService
	awsSvc      *awssvc.Service
	frontendURL string
	slack       SlackPoster
	enqueuer    SummaryEnqueuer
}

func NewService(db *models.DB, llmClient *llm.Client, email *notify.EmailService, awsSvc *awssvc.Service, frontendURL string) *Service {
	return &Service{db: db, llm: llmClient, email: email, awsSvc: awsSvc, frontendURL: strings.TrimRight(frontendURL, "/")}
}

func (s *Service) SetSlack(p SlackPoster)        { s.slack = p }
func (s *Service) SetEnqueuer(e SummaryEnqueuer) { s.enqueuer = e }

// DailySummary is the structured + rendered daily briefing for an org.
type DailySummary struct {
	OrgID           uuid.UUID `json:"org_id"`
	Date            time.Time `json:"date"`
	DeployCount     int       `json:"deploy_count"`
	DeploySucceeded int       `json:"deploy_succeeded"`
	DeployFailed    int       `json:"deploy_failed"`
	IncidentCount   int       `json:"incident_count"`
	IncidentsMTTR   float64   `json:"incidents_mttr"` // avg minutes to resolve (24h)
	OpenAlerts      int       `json:"open_alerts"`
	ResolvedAlerts  int       `json:"resolved_alerts"`
	CostChangePct   float64   `json:"cost_change_pct"` // vs previous 7-day avg (best-effort)
	TopFailures     []string  `json:"top_failures"`
	Recommendations []string  `json:"recommendations"`
	MarkdownContent string    `json:"markdown_content"`
}

// GenerateDailySummary gathers the last-24h metrics for an org, asks Claude for a plain
// summary + recommendations, renders markdown, and upserts daily_summaries.
func (s *Service) GenerateDailySummary(ctx context.Context, orgID uuid.UUID, date time.Time) (*DailySummary, error) {
	sum := &DailySummary{OrgID: orgID, Date: date}

	// 1. Deploys (last 24h) by status.
	_ = s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE d.status = 'live'),
		       COUNT(*) FILTER (WHERE d.status = 'failed')
		FROM deployments d JOIN projects p ON p.id = d.project_id
		WHERE p.org_id = $1 AND d.updated_at > NOW() - INTERVAL '24 hours'`, orgID,
	).Scan(&sum.DeployCount, &sum.DeploySucceeded, &sum.DeployFailed)

	// 2. Incidents created in last 24h + MTTR over those resolved.
	_ = s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COALESCE(AVG(EXTRACT(EPOCH FROM (resolved_at - created_at)) / 60.0)
		                FILTER (WHERE resolved_at IS NOT NULL), 0)
		FROM incidents
		WHERE org_id = $1 AND created_at > NOW() - INTERVAL '24 hours'`, orgID,
	).Scan(&sum.IncidentCount, &sum.IncidentsMTTR)

	// 3. Alerts: currently open + resolved in last 24h.
	_ = s.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM alerts WHERE org_id = $1 AND status = 'open'`, orgID).Scan(&sum.OpenAlerts)
	_ = s.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM alerts WHERE org_id = $1 AND resolved_at > NOW() - INTERVAL '24 hours'`, orgID).Scan(&sum.ResolvedAlerts)

	// 4. Top recurring failures from project memory (7d, by reference_count).
	frows, err := s.db.Pool.Query(ctx, `
		SELECT pm.content FROM project_memory pm JOIN projects p ON p.id = pm.project_id
		WHERE p.org_id = $1 AND pm.memory_type = 'recurring_failure'
		  AND pm.last_referenced_at > NOW() - INTERVAL '7 days'
		ORDER BY pm.reference_count DESC LIMIT 3`, orgID)
	if err == nil {
		for frows.Next() {
			var c string
			if frows.Scan(&c) == nil {
				sum.TopFailures = append(sum.TopFailures, c)
			}
		}
		frows.Close()
	}

	// 5. Cost change — best-effort; skipped for now (left 0). See CURRENT_STATE.
	sum.CostChangePct = s.costChange(ctx, orgID)

	// 6 + 7. Ask Claude for a grounded paragraph + recommendations.
	paragraph, recs := s.analyze(ctx, sum)
	sum.Recommendations = recs
	sum.MarkdownContent = renderMarkdown(sum, paragraph)

	// 8. Persist (idempotent per org+date).
	contentJSON, _ := json.Marshal(sum)
	// is_monthly = false: this is the daily briefing. Monthly operational health reports
	// (internal/analytics) share this table with is_monthly = true; the unique index keys on
	// all three columns (see addMonthlyFlagToDailySummaries), so the conflict target matches.
	_, perr := s.db.Pool.Exec(ctx, `
		INSERT INTO daily_summaries (org_id, summary_date, content_markdown, content_json, generated_at, is_monthly)
		VALUES ($1, $2, $3, $4, NOW(), false)
		ON CONFLICT (org_id, summary_date, is_monthly) DO UPDATE SET
		    content_markdown = EXCLUDED.content_markdown,
		    content_json = EXCLUDED.content_json,
		    generated_at = NOW()`,
		orgID, date.Format("2006-01-02"), sum.MarkdownContent, contentJSON)
	if perr != nil {
		return sum, fmt.Errorf("store daily summary: %w", perr)
	}
	return sum, nil
}

// costChange returns the last-7d-vs-prior-7d platform cost change percentage for the org,
// via Cost Explorer on the org's first AWS-funded project. Best-effort: returns 0 (omitted
// from output) when there's no connected account, no prior-period spend, or any API error.
func (s *Service) costChange(ctx context.Context, orgID uuid.UUID) float64 {
	if s.awsSvc == nil {
		return 0
	}
	var projectID uuid.UUID
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT id FROM projects WHERE org_id = $1 AND account_id IS NOT NULL ORDER BY created_at ASC LIMIT 1`,
		orgID).Scan(&projectID); err != nil {
		return 0
	}

	now := time.Now().UTC()
	fmtDate := func(t time.Time) string { return t.Format("2006-01-02") }
	last7, err := s.awsSvc.GetCostTotalForRange(ctx, projectID.String(), fmtDate(now.AddDate(0, 0, -7)), fmtDate(now))
	if err != nil {
		return 0
	}
	prior7, err := s.awsSvc.GetCostTotalForRange(ctx, projectID.String(), fmtDate(now.AddDate(0, 0, -14)), fmtDate(now.AddDate(0, 0, -7)))
	if err != nil || prior7 <= 0 {
		return 0
	}
	return (last7 - prior7) / prior7 * 100.0
}

// analyze asks Claude for a one-paragraph summary and ≤3 recommendations, grounded in the
// metrics. Returns a fallback paragraph + no recommendations on any failure.
func (s *Service) analyze(ctx context.Context, sum *DailySummary) (string, []string) {
	system := "You are an SRE writing a concise daily operations briefing for an engineering " +
		"team. Given the metrics, respond with STRICT JSON: " +
		`{"summary":"<one plain-English paragraph>","recommendations":["<rec>","<rec>","<rec>"]}. ` +
		"Base everything ONLY on the provided data — no generic advice. Max 3 recommendations; " +
		"fewer (or none) if the data doesn't support them."

	user := fmt.Sprintf(
		"Deploys (24h): %d total, %d succeeded, %d failed\nIncidents (24h): %d, avg MTTR %.0f min\n"+
			"Alerts: %d open, %d resolved (24h)\nTop recurring failures (7d): %s",
		sum.DeployCount, sum.DeploySucceeded, sum.DeployFailed, sum.IncidentCount, sum.IncidentsMTTR,
		sum.OpenAlerts, sum.ResolvedAlerts, strings.Join(sum.TopFailures, "; "))
	if sum.CostChangePct != 0 {
		user += fmt.Sprintf("\nCost change vs prior 7-day avg: %+.1f%%", sum.CostChangePct)
	}

	text, err := s.llm.Complete(ctx, system, user, 600)
	if err != nil {
		return s.fallbackParagraph(sum), nil
	}
	var parsed struct {
		Summary         string   `json:"summary"`
		Recommendations []string `json:"recommendations"`
	}
	if err := json.Unmarshal([]byte(stripFences(text)), &parsed); err != nil || parsed.Summary == "" {
		return s.fallbackParagraph(sum), nil
	}
	if len(parsed.Recommendations) > 3 {
		parsed.Recommendations = parsed.Recommendations[:3]
	}
	return parsed.Summary, parsed.Recommendations
}

func (s *Service) fallbackParagraph(sum *DailySummary) string {
	return fmt.Sprintf("In the last 24 hours: %d deploys (%d failed), %d incidents (avg MTTR %.0f min), and %d open alerts.",
		sum.DeployCount, sum.DeployFailed, sum.IncidentCount, sum.IncidentsMTTR, sum.OpenAlerts)
}

func renderMarkdown(sum *DailySummary, paragraph string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Daily summary — %s\n\n%s\n\n", sum.Date.Format("Jan 2, 2006"), paragraph)
	b.WriteString("**Activity**\n")
	fmt.Fprintf(&b, "- Deploys: %d (%d succeeded, %d failed)\n", sum.DeployCount, sum.DeploySucceeded, sum.DeployFailed)
	fmt.Fprintf(&b, "- Incidents: %d (avg MTTR %.0f min)\n", sum.IncidentCount, sum.IncidentsMTTR)
	fmt.Fprintf(&b, "- Alerts: %d open, %d resolved\n", sum.OpenAlerts, sum.ResolvedAlerts)
	if sum.CostChangePct != 0 {
		fmt.Fprintf(&b, "- Cost vs prior 7-day avg: %+.1f%%\n", sum.CostChangePct)
	}
	if len(sum.TopFailures) > 0 {
		b.WriteString("\n**Top recurring failures**\n")
		for _, f := range sum.TopFailures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(sum.Recommendations) > 0 {
		b.WriteString("\n**Recommendations**\n")
		for _, r := range sum.Recommendations {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	return b.String()
}

// GenerateAndDeliver generates a summary and delivers it (Slack + email). Used by the
// generation job and the manual trigger.
func (s *Service) GenerateAndDeliver(ctx context.Context, orgID uuid.UUID, date time.Time) (*DailySummary, error) {
	sum, err := s.GenerateDailySummary(ctx, orgID, date)
	if err != nil {
		return sum, err
	}
	s.DeliverSummary(ctx, sum)
	return sum, nil
}

// DeliverSummary posts to Slack (if connected) and emails admins/engineers who opted into
// notifications, then records the delivery flags. Best-effort.
func (s *Service) DeliverSummary(ctx context.Context, sum *DailySummary) {
	var orgName string
	_ = s.db.Pool.QueryRow(ctx, `SELECT name FROM organizations WHERE id = $1`, sum.OrgID).Scan(&orgName)
	url := s.frontendURL + "/orgs/" + sum.OrgID.String() + "/summaries"

	deliveredSlack := false
	if s.slack != nil {
		if ok, err := s.slack.PostDailySummary(ctx, sum.OrgID, sum.MarkdownContent); err != nil {
			slog.Warn("summary: slack delivery failed", "component", "summary", "org", sum.OrgID, "error", err)
		} else {
			deliveredSlack = ok
		}
	}

	deliveredEmail := false
	if s.email != nil {
		paragraph := firstParagraph(sum.MarkdownContent)
		rows, err := s.db.Pool.Query(ctx, `
			SELECT u.email FROM organization_members m JOIN users u ON u.id = m.user_id
			WHERE m.org_id = $1 AND m.role IN ('admin','engineer')
			  AND u.notifications_enabled AND u.notify_alert_fired`, sum.OrgID)
		if err == nil {
			for rows.Next() {
				var email string
				if rows.Scan(&email) != nil || email == "" {
					continue
				}
				if err := s.email.SendDailySummary(ctx, email, orgName, sum.Date.Format("Jan 2, 2006"), paragraph, sum.Recommendations, url); err == nil {
					deliveredEmail = true
				}
			}
			rows.Close()
		}
	}

	_, _ = s.db.Pool.Exec(ctx,
		`UPDATE daily_summaries SET delivered_slack = $1, delivered_email = $2
		 WHERE org_id = $3 AND summary_date = $4 AND is_monthly = false`,
		deliveredSlack, deliveredEmail, sum.OrgID, sum.Date.Format("2006-01-02"))
}

// EnqueueDueSummaries enqueues a generation job for every enabled org whose configured
// delivery hour matches the current hour in its timezone and that has no summary yet for
// its local date today. Called hourly.
func (s *Service) EnqueueDueSummaries(ctx context.Context) error {
	if s.enqueuer == nil {
		return nil
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, to_char(summary_time, 'HH24'), summary_timezone
		FROM organizations WHERE summary_enabled = true`)
	if err != nil {
		return err
	}
	type due struct {
		id   uuid.UUID
		hour string
		tz   string
	}
	var orgsList []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.hour, &d.tz) == nil {
			orgsList = append(orgsList, d)
		}
	}
	rows.Close()

	now := time.Now()
	for _, o := range orgsList {
		loc, err := time.LoadLocation(o.tz)
		if err != nil {
			loc = time.UTC
		}
		local := now.In(loc)
		if fmt.Sprintf("%02d", local.Hour()) != o.hour {
			continue
		}
		// Skip if a summary for the org-local date already exists.
		var exists bool
		_ = s.db.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM daily_summaries WHERE org_id = $1 AND summary_date = $2)`,
			o.id, local.Format("2006-01-02")).Scan(&exists)
		if exists {
			continue
		}
		if err := s.enqueuer.EnqueueGenerateSummary(o.id.String()); err != nil {
			slog.Warn("summary: enqueue failed", "component", "summary", "org", o.id, "error", err)
		}
	}
	return nil
}

// stripFences removes ```json fences the model sometimes wraps around JSON.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// firstParagraph returns the text after the markdown H1/H2 heading (the summary prose).
func firstParagraph(md string) string {
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "**") || strings.HasPrefix(t, "-") {
			continue
		}
		return t
	}
	return md
}
