package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

// MonthlyReport is the structured monthly operational health report for an org. It is
// stored in daily_summaries with is_monthly = true (content_json = this struct).
type MonthlyReport struct {
	OrgID            uuid.UUID                      `json:"org_id"`
	Month            string                         `json:"month"` // YYYY-MM
	Metrics          *models.ReliabilityMetrics     `json:"metrics"`
	Projects         []models.ProjectReliabilityRow `json:"projects"`
	IncidentCount    int                            `json:"incident_count"`
	PostmortemCount  int                            `json:"postmortem_count"`
	DeployCount      int                            `json:"deploy_count"`
	DeploySucceeded  int                            `json:"deploy_succeeded"`
	DeployFailed     int                            `json:"deploy_failed"`
	ExecutiveSummary string                         `json:"executive_summary"`
	MarkdownContent  string                         `json:"markdown_content"`
}

// GenerateMonthlyReport builds the monthly operational health report for an org over the
// calendar month containing `month`, stores it (daily_summaries, is_monthly=true), and
// emails it to the org's admins. Returns the report.
func (s *AnalyticsService) GenerateMonthlyReport(ctx context.Context, orgID uuid.UUID, month time.Time) (*MonthlyReport, error) {
	monthStart := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	days := int(monthEnd.Sub(monthStart).Hours() / 24)

	rep := &MonthlyReport{OrgID: orgID, Month: monthStart.Format("2006-01")}

	// 1. Reliability metrics for the month (org-wide) + per-project breakdown.
	metrics, _ := s.GetReliabilityMetrics(ctx, orgID, nil, days)
	rep.Metrics = metrics
	rep.Projects, _ = s.GetProjectBreakdown(ctx, orgID, days)

	// 2. Incidents + postmortems for the month.
	_ = s.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM incidents WHERE org_id = $1 AND created_at >= $2 AND created_at < $3`,
		orgID, monthStart, monthEnd).Scan(&rep.IncidentCount)
	_ = s.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM postmortems WHERE org_id = $1 AND created_at >= $2 AND created_at < $3`,
		orgID, monthStart, monthEnd).Scan(&rep.PostmortemCount)

	// 3. Deployment statistics for the month.
	_ = s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE d.status IN ('live','failed')),
		       COUNT(*) FILTER (WHERE d.status = 'live'),
		       COUNT(*) FILTER (WHERE d.status = 'failed')
		FROM deployments d JOIN projects p ON p.id = d.project_id
		WHERE p.org_id = $1 AND d.updated_at >= $2 AND d.updated_at < $3`,
		orgID, monthStart, monthEnd).Scan(&rep.DeployCount, &rep.DeploySucceeded, &rep.DeployFailed)

	// 4. Cost data if any AWS account is connected (best-effort).
	costLine := s.monthlyCostLine(ctx, orgID)

	// 5. Executive summary from Claude (factual, no fluff).
	rep.ExecutiveSummary = s.executiveSummary(ctx, rep, costLine)

	// 6. Render + store (is_monthly = true; summary_date = first of month).
	rep.MarkdownContent = renderReportMarkdown(rep, costLine)
	if _, err := s.db.Pool.Exec(ctx, `
		INSERT INTO daily_summaries (org_id, summary_date, content_markdown, content_json, generated_at, is_monthly)
		VALUES ($1, $2, $3, $4, NOW(), true)
		ON CONFLICT (org_id, summary_date, is_monthly) DO UPDATE SET
		    content_markdown = EXCLUDED.content_markdown,
		    content_json = EXCLUDED.content_json,
		    generated_at = NOW()`,
		orgID, monthStart.Format("2006-01-02"), rep.MarkdownContent, marshalJSON(rep)); err != nil {
		return rep, fmt.Errorf("store monthly report: %w", err)
	}

	// 7. Deliver: email admins + post to Slack (best-effort).
	s.deliverReport(ctx, rep)
	return rep, nil
}

// GenerateAllMonthlyReports generates last month's report for every org. Run by the
// scheduler on the 1st. Best-effort per org.
func (s *AnalyticsService) GenerateAllMonthlyReports(ctx context.Context) error {
	rows, err := s.db.Pool.Query(ctx, `SELECT id FROM organizations`)
	if err != nil {
		return err
	}
	var orgIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			orgIDs = append(orgIDs, id)
		}
	}
	rows.Close()

	lastMonth := time.Now().UTC().AddDate(0, 0, -1) // a day in the previous month
	for _, id := range orgIDs {
		if _, err := s.GenerateMonthlyReport(ctx, id, lastMonth); err != nil {
			slog.Warn("analytics: monthly report failed", "component", "analytics", "org", id, "error", err)
		}
	}
	return nil
}

// monthlyCostLine returns a human cost line for the org (via any project funded by a
// connected account), or "" when no account/Cost Explorer data is available. Best-effort —
// GetAccountCostSummary resolves the account from the project.
func (s *AnalyticsService) monthlyCostLine(ctx context.Context, orgID uuid.UUID) string {
	if s.awsSvc == nil {
		return ""
	}
	var projectID uuid.UUID
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT id FROM projects WHERE org_id = $1 AND account_id IS NOT NULL ORDER BY created_at ASC LIMIT 1`,
		orgID).Scan(&projectID); err != nil {
		return ""
	}
	cost, err := s.awsSvc.GetAccountCostSummary(ctx, projectID.String())
	if err != nil || cost == nil {
		return ""
	}
	return fmt.Sprintf("%s%.2f over the last 30 days", currencySymbol(cost.Currency), cost.TotalMonthlyCost)
}

func currencySymbol(c string) string {
	if c == "" || c == "USD" {
		return "$"
	}
	return c + " "
}

// executiveSummary asks Claude for a 1–2 paragraph factual summary. Falls back to a
// deterministic sentence on any failure.
func (s *AnalyticsService) executiveSummary(ctx context.Context, rep *MonthlyReport, costLine string) string {
	if s.llm == nil {
		return s.fallbackSummary(rep)
	}
	system := "You are writing the executive summary of a monthly operational health report for " +
		"engineering leadership (VPs/CTOs). Write 1-2 short paragraphs, factual and specific to the " +
		"numbers given. Highlight genuine wins, flag real risks. No fluff, no generic advice, no " +
		"bullet points — plain prose only. If a metric is healthy, say so plainly."

	m := rep.Metrics
	user := fmt.Sprintf(
		"Month: %s\nUptime: %.3f%% (SLA target %.2f%%, status %s)\nIncidents: %d\nPostmortems: %d\n"+
			"MTTD: %.0f min, MTTR: %.0f min\nDeploys: %d (%d succeeded, %d failed)\n"+
			"Deploy success rate: %.1f%%\nChange failure rate: %.1f%%",
		rep.Month, m.UptimePct, m.SLATarget, m.SLAStatus, rep.IncidentCount, rep.PostmortemCount,
		m.MTTD, m.MTTR, rep.DeployCount, rep.DeploySucceeded, rep.DeployFailed,
		m.DeploySuccessRate, m.ChangeFailureRate)
	if costLine != "" {
		user += "\nCloud cost: " + costLine
	}
	if len(m.TopFailurePatterns) > 0 {
		var ps []string
		for _, fp := range m.TopFailurePatterns {
			ps = append(ps, fp.Pattern)
		}
		user += "\nTop recurring failures: " + strings.Join(ps, "; ")
	}

	text, err := s.llm.Complete(ctx, system, user, 600)
	if err != nil || strings.TrimSpace(text) == "" {
		return s.fallbackSummary(rep)
	}
	return strings.TrimSpace(text)
}

func (s *AnalyticsService) fallbackSummary(rep *MonthlyReport) string {
	m := rep.Metrics
	return fmt.Sprintf(
		"In %s, average uptime was %.2f%% against a %.2f%% SLA target (%s). There were %d incidents "+
			"with an average MTTR of %.0f minutes, and %d deploys (%.0f%% successful).",
		rep.Month, m.UptimePct, m.SLATarget, strings.ReplaceAll(m.SLAStatus, "_", " "),
		rep.IncidentCount, m.MTTR, rep.DeployCount, m.DeploySuccessRate)
}

func renderReportMarkdown(rep *MonthlyReport, costLine string) string {
	m := rep.Metrics
	var b strings.Builder
	fmt.Fprintf(&b, "# Monthly operational health report — %s\n\n%s\n\n", rep.Month, rep.ExecutiveSummary)

	b.WriteString("## Reliability\n")
	fmt.Fprintf(&b, "- Uptime: **%.3f%%** (SLA target %.2f%%, %s)\n", m.UptimePct, m.SLATarget, strings.ReplaceAll(m.SLAStatus, "_", " "))
	fmt.Fprintf(&b, "- MTTD: %.0f min · MTTR: %.0f min\n", m.MTTD, m.MTTR)
	fmt.Fprintf(&b, "- Incidents: %d · Postmortems: %d\n", rep.IncidentCount, rep.PostmortemCount)
	fmt.Fprintf(&b, "- Deploys: %d (%d succeeded, %d failed) · success rate %.1f%% · change-failure rate %.1f%%\n",
		rep.DeployCount, rep.DeploySucceeded, rep.DeployFailed, m.DeploySuccessRate, m.ChangeFailureRate)
	if costLine != "" {
		fmt.Fprintf(&b, "- Cloud cost: %s\n", costLine)
	}

	if len(rep.Projects) > 0 {
		b.WriteString("\n## By project / environment\n")
		b.WriteString("| Project | Environment | Uptime | MTTR | Incidents | SLA |\n|---|---|---|---|---|---|\n")
		for _, p := range rep.Projects {
			fmt.Fprintf(&b, "| %s | %s | %.2f%% | %.0f min | %d | %s |\n",
				p.ProjectName, p.EnvironmentName, p.UptimePct, p.MTTR, p.IncidentCount, strings.ReplaceAll(p.SLAStatus, "_", " "))
		}
	}

	if len(m.TopFailurePatterns) > 0 {
		b.WriteString("\n## Top recurring failures\n")
		for _, fp := range m.TopFailurePatterns {
			fmt.Fprintf(&b, "- %s (×%d)\n", fp.Pattern, fp.Count)
		}
	}
	return b.String()
}

// deliverReport emails admins and posts to Slack. Best-effort.
func (s *AnalyticsService) deliverReport(ctx context.Context, rep *MonthlyReport) {
	var orgName string
	_ = s.db.Pool.QueryRow(ctx, `SELECT name FROM organizations WHERE id = $1`, rep.OrgID).Scan(&orgName)
	url := s.frontendURL + "/orgs/" + rep.OrgID.String() + "/analytics"

	if s.slack != nil {
		if _, err := s.slack.PostDailySummary(ctx, rep.OrgID, rep.MarkdownContent); err != nil {
			slog.Warn("analytics: slack report delivery failed", "component", "analytics", "org", rep.OrgID, "error", err)
		}
	}

	if s.email != nil {
		rows, err := s.db.Pool.Query(ctx, `
			SELECT u.email FROM organization_members m JOIN users u ON u.id = m.user_id
			WHERE m.org_id = $1 AND m.role = 'admin'
			  AND u.notifications_enabled`, rep.OrgID)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var email string
			if rows.Scan(&email) != nil || email == "" {
				continue
			}
			_ = s.email.SendMonthlyReport(ctx, email, orgName, rep.Month, rep.ExecutiveSummary, url)
		}
	}
}
