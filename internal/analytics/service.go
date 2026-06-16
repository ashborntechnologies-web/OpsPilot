// Package analytics powers the engineering-leadership dashboard: SLA/uptime tracking,
// MTTD/MTTR, reliability trends, and the monthly operational health report.
//
// Uptime is computed from operational events (runtime.service_down /
// runtime.service_recovered), not from external monitoring probes — see ADR-015. The
// daily job snapshots each environment into uptime_snapshots; the dashboard aggregates
// those snapshots plus incident/deployment data into ReliabilityMetrics.
package analytics

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

// SlackPoster posts a markdown report to an org's Slack summary channel. Satisfied by
// *slack.Service (injected via SetSlack to avoid an import cycle).
type SlackPoster interface {
	PostDailySummary(ctx context.Context, orgID uuid.UUID, markdown string) (bool, error)
}

// AnalyticsService computes reliability metrics and generates the monthly health report.
type AnalyticsService struct {
	db          *models.DB
	llm         *llm.Client
	email       *notify.EmailService
	awsSvc      *awssvc.Service
	frontendURL string
	slack       SlackPoster
}

func NewService(db *models.DB, llmClient *llm.Client, email *notify.EmailService, awsSvc *awssvc.Service, frontendURL string) *AnalyticsService {
	return &AnalyticsService{db: db, llm: llmClient, email: email, awsSvc: awsSvc, frontendURL: strings.TrimRight(frontendURL, "/")}
}

func (s *AnalyticsService) SetSlack(p SlackPoster) { s.slack = p }

// defaultSLATarget is the industry-common "three nines" used when no SLA is configured.
const defaultSLATarget = 99.9

// ───────────────────────── uptime snapshots ─────────────────────────

// ComputeUptimeSnapshot computes the uptime for one environment on one (UTC) day from its
// runtime.service_down / runtime.service_recovered events and upserts uptime_snapshots. It
// is idempotent per (environment, date) so the daily job can safely recompute.
//
// Downtime is the union of [down → recovered] intervals clipped to the day. A downtime that
// began on a previous day (no intervening recovery) is carried in by replaying all prior
// events, so an outage spanning midnight is attributed correctly to each day.
func (s *AnalyticsService) ComputeUptimeSnapshot(ctx context.Context, environmentID uuid.UUID, date time.Time) error {
	dayStart := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	// Cap the window at "now" so today's partial day reports honest totals.
	windowEnd := dayEnd
	if now := time.Now().UTC(); now.Before(dayEnd) {
		windowEnd = now
	}
	if !windowEnd.After(dayStart) {
		return nil // future date — nothing to compute
	}

	rows, err := s.db.Pool.Query(ctx, `
		SELECT event_type, occurred_at
		FROM operational_events
		WHERE environment_id = $1
		  AND event_type IN ($2, $3)
		  AND occurred_at < $4
		ORDER BY occurred_at ASC`,
		environmentID, models.EventRuntimeServiceDown, models.EventRuntimeServiceRecovered, windowEnd)
	if err != nil {
		return fmt.Errorf("query uptime events: %w", err)
	}
	defer rows.Close()

	var downtime time.Duration
	down := false
	var downSince time.Time
	for rows.Next() {
		var evType string
		var at time.Time
		if rows.Scan(&evType, &at) != nil {
			continue
		}
		switch evType {
		case models.EventRuntimeServiceDown:
			if !down {
				down = true
				downSince = at
			}
		case models.EventRuntimeServiceRecovered:
			if down {
				downtime += overlap(downSince, at, dayStart, windowEnd)
				down = false
			}
		}
	}
	// Still down at the end of the window → count up to windowEnd.
	if down {
		downtime += overlap(downSince, windowEnd, dayStart, windowEnd)
	}

	totalMinutes := int(windowEnd.Sub(dayStart).Minutes())
	downtimeMinutes := int(downtime.Minutes())
	if downtimeMinutes > totalMinutes {
		downtimeMinutes = totalMinutes
	}
	uptimePct := 100.0
	if totalMinutes > 0 {
		uptimePct = float64(totalMinutes-downtimeMinutes) / float64(totalMinutes) * 100.0
	}

	var incidentCount int
	_ = s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM incidents
		WHERE environment_id = $1 AND created_at >= $2 AND created_at < $3`,
		environmentID, dayStart, dayEnd).Scan(&incidentCount)

	_, err = s.db.Pool.Exec(ctx, `
		INSERT INTO uptime_snapshots
		    (environment_id, snapshot_date, total_minutes, downtime_minutes, uptime_pct, incident_count)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (environment_id, snapshot_date) DO UPDATE SET
		    total_minutes = EXCLUDED.total_minutes,
		    downtime_minutes = EXCLUDED.downtime_minutes,
		    uptime_pct = EXCLUDED.uptime_pct,
		    incident_count = EXCLUDED.incident_count`,
		environmentID, dayStart.Format("2006-01-02"), totalMinutes, downtimeMinutes, uptimePct, incidentCount)
	if err != nil {
		return fmt.Errorf("upsert uptime snapshot: %w", err)
	}
	return nil
}

// SnapshotAllEnvironments computes yesterday's and today's uptime snapshot for every
// non-preview environment. Run daily by the scheduler. Recomputing today keeps the
// dashboard fresh; yesterday is recomputed once so a late-arriving recovery event is
// captured. Best-effort: one environment failing does not stop the rest.
func (s *AnalyticsService) SnapshotAllEnvironments(ctx context.Context) error {
	rows, err := s.db.Pool.Query(ctx, `SELECT id FROM environments WHERE is_preview = false`)
	if err != nil {
		return err
	}
	var envIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			envIDs = append(envIDs, id)
		}
	}
	rows.Close()

	now := time.Now().UTC()
	yesterday := now.Add(-24 * time.Hour)
	var failures int
	for _, id := range envIDs {
		for _, d := range []time.Time{yesterday, now} {
			if err := s.ComputeUptimeSnapshot(ctx, id, d); err != nil {
				failures++
				slog.Warn("analytics: snapshot failed", "component", "analytics", "env", id, "date", d.Format("2006-01-02"), "error", err)
			}
		}
	}
	slog.Info("analytics: daily snapshot complete", "component", "analytics", "environments", len(envIDs), "failures", failures)
	return nil
}

// overlap returns the duration of [aStart,aEnd) intersected with [wStart,wEnd).
func overlap(aStart, aEnd, wStart, wEnd time.Time) time.Duration {
	start := aStart
	if start.Before(wStart) {
		start = wStart
	}
	end := aEnd
	if end.After(wEnd) {
		end = wEnd
	}
	if end.After(start) {
		return end.Sub(start)
	}
	return 0
}

// ───────────────────────── reliability metrics ─────────────────────────

// scope is the project/org filter for a metrics query. A nil projectID means org-wide.
type scope struct {
	orgID     uuid.UUID
	projectID *uuid.UUID
}

// projectFilter returns a SQL predicate + args fragment for the scope, applied to a query
// where `p` aliases projects (or a table with project_id/org_id). The caller supplies the
// starting placeholder index.
func (sc scope) clause(col string, startIdx int) (string, []any) {
	// col is the org_id column expression (e.g. "p.org_id").
	args := []any{sc.orgID}
	clause := fmt.Sprintf("%s = $%d", col, startIdx)
	if sc.projectID != nil {
		args = append(args, *sc.projectID)
	}
	return clause, args
}

// GetReliabilityMetrics computes the leadership metrics for an org (projectID == nil) or a
// single project, over the last `days`. It also computes the immediately-preceding window
// for trend arrows.
func (s *AnalyticsService) GetReliabilityMetrics(ctx context.Context, orgID uuid.UUID, projectID *uuid.UUID, days int) (*models.ReliabilityMetrics, error) {
	if days <= 0 {
		days = 30
	}
	sc := scope{orgID: orgID, projectID: projectID}
	now := time.Now().UTC()
	curSince := now.AddDate(0, 0, -days)
	prevSince := curSince.AddDate(0, 0, -days)

	m := &models.ReliabilityMetrics{Days: days}

	// Uptime (current + previous) from snapshots.
	m.UptimePct = s.windowUptime(ctx, sc, curSince, now)
	m.PrevUptimePct = s.windowUptime(ctx, sc, prevSince, curSince)

	// SLA target + status.
	m.SLATarget = s.slaTarget(ctx, sc)
	m.SLAStatus = SLAStatus(m.UptimePct, m.SLATarget)

	// MTTD / MTTR (current + previous).
	m.MTTD = s.mttd(ctx, sc, curSince, now)
	m.PrevMTTD = s.mttd(ctx, sc, prevSince, curSince)
	m.MTTR = s.mttr(ctx, sc, curSince, now)
	m.PrevMTTR = s.mttr(ctx, sc, prevSince, curSince)

	// Incident count.
	m.IncidentCount = s.incidentCount(ctx, sc, curSince, now)

	// Deploy stats (current + previous).
	m.DeployCount, m.DeploySuccessRate, m.ChangeFailureRate = s.deployStats(ctx, sc, curSince, now)
	_, m.PrevDeploySuccessRate, _ = s.deployStats(ctx, sc, prevSince, curSince)

	// Trends.
	m.UptimeTrend = s.uptimeTrend(ctx, sc, days)
	m.IncidentTrend = s.incidentTrend(ctx, sc, 12)
	m.TopFailurePatterns = s.topFailurePatterns(ctx, sc, 5)

	return m, nil
}

// windowUptime returns the minutes-weighted uptime percentage over [since,until) from
// uptime_snapshots for the scope. Defaults to 100 when there are no snapshots.
func (s *AnalyticsService) windowUptime(ctx context.Context, sc scope, since, until time.Time) float64 {
	orgClause, args := sc.clause("p.org_id", 1)
	q := `
		SELECT COALESCE(SUM(us.total_minutes), 0), COALESCE(SUM(us.downtime_minutes), 0)
		FROM uptime_snapshots us
		JOIN environments e ON e.id = us.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE ` + orgClause + `
		  AND us.snapshot_date >= $%d::date AND us.snapshot_date <= $%d::date`
	if sc.projectID != nil {
		q += " AND p.id = $2"
	}
	args = append(args, since, until)
	q = fmt.Sprintf(q, len(args)-1, len(args))

	var total, downtime int
	if err := s.db.Pool.QueryRow(ctx, q, args...).Scan(&total, &downtime); err != nil || total == 0 {
		return 100.0
	}
	return float64(total-downtime) / float64(total) * 100.0
}

// slaTarget returns the average configured SLA target for the scope (default 99.9 if none).
func (s *AnalyticsService) slaTarget(ctx context.Context, sc scope) float64 {
	orgClause, args := sc.clause("org_id", 1)
	q := `SELECT AVG(target_uptime_pct) FROM service_slas WHERE ` + orgClause
	if sc.projectID != nil {
		q += " AND project_id = $2"
	}
	var target *float64
	if err := s.db.Pool.QueryRow(ctx, q, args...).Scan(&target); err != nil || target == nil {
		return defaultSLATarget
	}
	return *target
}

// mttd computes the average minutes from the first error event (within 10 minutes before an
// incident opened, in the incident's environment/project) to acknowledgement.
func (s *AnalyticsService) mttd(ctx context.Context, sc scope, since, until time.Time) float64 {
	orgClause, args := sc.clause("i.org_id", 1)
	projFilter := ""
	if sc.projectID != nil {
		projFilter = " AND i.project_id = $2"
	}
	args = append(args, since, until)
	q := fmt.Sprintf(`
		SELECT COALESCE(AVG(EXTRACT(EPOCH FROM (i.acknowledged_at - ev.first_err)) / 60.0), 0)
		FROM incidents i
		JOIN LATERAL (
		    SELECT MIN(oe.occurred_at) AS first_err
		    FROM operational_events oe
		    WHERE oe.severity = 'error'
		      AND oe.occurred_at BETWEEN i.created_at - INTERVAL '10 minutes' AND i.created_at
		      AND (oe.environment_id = i.environment_id
		           OR (i.environment_id IS NULL AND oe.project_id = i.project_id))
		) ev ON TRUE
		WHERE %s%s
		  AND i.acknowledged_at IS NOT NULL
		  AND ev.first_err IS NOT NULL
		  AND i.created_at >= $%d AND i.created_at < $%d`,
		orgClause, projFilter, len(args)-1, len(args))

	var mttd float64
	_ = s.db.Pool.QueryRow(ctx, q, args...).Scan(&mttd)
	return mttd
}

// mttr computes the average minutes from acknowledgement to resolution.
func (s *AnalyticsService) mttr(ctx context.Context, sc scope, since, until time.Time) float64 {
	orgClause, args := sc.clause("i.org_id", 1)
	projFilter := ""
	if sc.projectID != nil {
		projFilter = " AND i.project_id = $2"
	}
	args = append(args, since, until)
	q := fmt.Sprintf(`
		SELECT COALESCE(AVG(EXTRACT(EPOCH FROM (i.resolved_at - i.acknowledged_at)) / 60.0), 0)
		FROM incidents i
		WHERE %s%s
		  AND i.acknowledged_at IS NOT NULL AND i.resolved_at IS NOT NULL
		  AND i.resolved_at >= $%d AND i.resolved_at < $%d`,
		orgClause, projFilter, len(args)-1, len(args))

	var mttr float64
	_ = s.db.Pool.QueryRow(ctx, q, args...).Scan(&mttr)
	return mttr
}

func (s *AnalyticsService) incidentCount(ctx context.Context, sc scope, since, until time.Time) int {
	orgClause, args := sc.clause("i.org_id", 1)
	projFilter := ""
	if sc.projectID != nil {
		projFilter = " AND i.project_id = $2"
	}
	args = append(args, since, until)
	q := fmt.Sprintf(`SELECT COUNT(*) FROM incidents i WHERE %s%s AND i.created_at >= $%d AND i.created_at < $%d`,
		orgClause, projFilter, len(args)-1, len(args))
	var n int
	_ = s.db.Pool.QueryRow(ctx, q, args...).Scan(&n)
	return n
}

// deployStats returns total completed deploys, deploy success rate (%), and change-failure
// rate (% of completed deploys with an incident opened within 30 minutes of completion).
func (s *AnalyticsService) deployStats(ctx context.Context, sc scope, since, until time.Time) (int, float64, float64) {
	orgClause, args := sc.clause("p.org_id", 1)
	projFilter := ""
	if sc.projectID != nil {
		projFilter = " AND p.id = $2"
	}
	args = append(args, since, until)
	q := fmt.Sprintf(`
		SELECT
		    COUNT(*) FILTER (WHERE d.status IN ('live','failed')),
		    COUNT(*) FILTER (WHERE d.status = 'live'),
		    COUNT(*) FILTER (WHERE d.status IN ('live','failed') AND EXISTS (
		        SELECT 1 FROM incidents i
		        WHERE i.project_id = d.project_id
		          AND (i.environment_id = d.environment_id OR i.environment_id IS NULL)
		          AND i.created_at BETWEEN d.updated_at AND d.updated_at + INTERVAL '30 minutes'
		    ))
		FROM deployments d
		JOIN projects p ON p.id = d.project_id
		WHERE %s%s AND d.updated_at >= $%d AND d.updated_at < $%d`,
		orgClause, projFilter, len(args)-1, len(args))

	var completed, succeeded, caused int
	_ = s.db.Pool.QueryRow(ctx, q, args...).Scan(&completed, &succeeded, &caused)
	if completed == 0 {
		return 0, 0, 0
	}
	return completed,
		float64(succeeded) / float64(completed) * 100.0,
		float64(caused) / float64(completed) * 100.0
}

// uptimeTrend returns daily uptime points over the last `days` (oldest first).
func (s *AnalyticsService) uptimeTrend(ctx context.Context, sc scope, days int) []models.UptimePoint {
	orgClause, args := sc.clause("p.org_id", 1)
	projFilter := ""
	if sc.projectID != nil {
		projFilter = " AND p.id = $2"
	}
	since := time.Now().UTC().AddDate(0, 0, -days)
	args = append(args, since)
	q := fmt.Sprintf(`
		SELECT us.snapshot_date::text,
		       CASE WHEN SUM(us.total_minutes) > 0
		            THEN (SUM(us.total_minutes) - SUM(us.downtime_minutes))::float / SUM(us.total_minutes) * 100
		            ELSE 100 END
		FROM uptime_snapshots us
		JOIN environments e ON e.id = us.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE %s%s AND us.snapshot_date >= $%d::date
		GROUP BY us.snapshot_date
		ORDER BY us.snapshot_date ASC`,
		orgClause, projFilter, len(args))

	out := []models.UptimePoint{}
	rows, err := s.db.Pool.Query(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p models.UptimePoint
		if rows.Scan(&p.Date, &p.UptimePct) == nil {
			out = append(out, p)
		}
	}
	return out
}

// incidentTrend returns incident counts bucketed by ISO week over the last `weeks` (oldest
// first), including zero-count weeks so the chart has a continuous x-axis.
func (s *AnalyticsService) incidentTrend(ctx context.Context, sc scope, weeks int) []models.IncidentPoint {
	orgClause, args := sc.clause("i.org_id", 1)
	projFilter := ""
	if sc.projectID != nil {
		projFilter = " AND i.project_id = $2"
	}
	since := time.Now().UTC().AddDate(0, 0, -weeks*7)
	args = append(args, since)
	q := fmt.Sprintf(`
		SELECT to_char(date_trunc('week', i.created_at), 'YYYY-MM-DD'), COUNT(*)
		FROM incidents i
		WHERE %s%s AND i.created_at >= $%d
		GROUP BY 1 ORDER BY 1 ASC`,
		orgClause, projFilter, len(args))

	counts := map[string]int{}
	rows, err := s.db.Pool.Query(ctx, q, args...)
	if err == nil {
		for rows.Next() {
			var wk string
			var n int
			if rows.Scan(&wk, &n) == nil {
				counts[wk] = n
			}
		}
		rows.Close()
	}

	// Build a continuous series of Monday-aligned weeks.
	out := make([]models.IncidentPoint, 0, weeks)
	monday := mondayOf(time.Now().UTC()).AddDate(0, 0, -(weeks-1)*7)
	for i := 0; i < weeks; i++ {
		key := monday.Format("2006-01-02")
		out = append(out, models.IncidentPoint{WeekStart: key, Count: counts[key]})
		monday = monday.AddDate(0, 0, 7)
	}
	return out
}

// topFailurePatterns returns recurring failures from project memory for the scope, ranked
// by reference count.
func (s *AnalyticsService) topFailurePatterns(ctx context.Context, sc scope, limit int) []models.FailurePattern {
	orgClause, args := sc.clause("p.org_id", 1)
	projFilter := ""
	if sc.projectID != nil {
		projFilter = " AND p.id = $2"
	}
	mtIdx := len(args) + 1  // memory_type placeholder
	limIdx := len(args) + 2 // limit placeholder
	args = append(args, models.MemoryRecurringFailure, limit)
	q := fmt.Sprintf(`
		SELECT pm.content, pm.reference_count
		FROM project_memory pm
		JOIN projects p ON p.id = pm.project_id
		WHERE %s%s AND pm.memory_type = $%d
		ORDER BY pm.reference_count DESC, pm.last_referenced_at DESC
		LIMIT $%d`,
		orgClause, projFilter, mtIdx, limIdx)

	out := []models.FailurePattern{}
	rows, err := s.db.Pool.Query(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var fp models.FailurePattern
		if rows.Scan(&fp.Pattern, &fp.Count) == nil {
			out = append(out, fp)
		}
	}
	return out
}

// GetProjectBreakdown returns per-environment reliability rows for the org dashboard table.
func (s *AnalyticsService) GetProjectBreakdown(ctx context.Context, orgID uuid.UUID, days int) ([]models.ProjectReliabilityRow, error) {
	if days <= 0 {
		days = 30
	}
	since := time.Now().UTC().AddDate(0, 0, -days)
	rows, err := s.db.Pool.Query(ctx, `
		SELECT p.id, p.name, e.id, e.name,
		       COALESCE(up.uptime_pct, 100),
		       COALESCE(mt.mttr, 0),
		       COALESCE(ic.cnt, 0),
		       COALESCE(sla.target_uptime_pct, $3)
		FROM environments e
		JOIN projects p ON p.id = e.project_id
		LEFT JOIN service_slas sla ON sla.environment_id = e.id
		LEFT JOIN LATERAL (
		    SELECT CASE WHEN SUM(total_minutes) > 0
		                THEN (SUM(total_minutes) - SUM(downtime_minutes))::float / SUM(total_minutes) * 100
		                ELSE 100 END AS uptime_pct
		    FROM uptime_snapshots us WHERE us.environment_id = e.id AND us.snapshot_date >= $2::date
		) up ON TRUE
		LEFT JOIN LATERAL (
		    SELECT AVG(EXTRACT(EPOCH FROM (i.resolved_at - i.acknowledged_at)) / 60.0) AS mttr
		    FROM incidents i WHERE i.environment_id = e.id
		      AND i.acknowledged_at IS NOT NULL AND i.resolved_at IS NOT NULL AND i.resolved_at >= $2
		) mt ON TRUE
		LEFT JOIN LATERAL (
		    SELECT COUNT(*) AS cnt FROM incidents i WHERE i.environment_id = e.id AND i.created_at >= $2
		) ic ON TRUE
		WHERE p.org_id = $1 AND e.is_preview = false
		ORDER BY p.name, e.name`,
		orgID, since, defaultSLATarget)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.ProjectReliabilityRow{}
	for rows.Next() {
		var r models.ProjectReliabilityRow
		if rows.Scan(&r.ProjectID, &r.ProjectName, &r.EnvironmentID, &r.EnvironmentName,
			&r.UptimePct, &r.MTTR, &r.IncidentCount, &r.SLATarget) != nil {
			continue
		}
		r.SLAStatus = SLAStatus(r.UptimePct, r.SLATarget)
		out = append(out, r)
	}
	return out, nil
}

// GetUptimeHistory returns daily uptime points for a project over `days` (for the chart).
func (s *AnalyticsService) GetUptimeHistory(ctx context.Context, projectID uuid.UUID, days int) []models.UptimePoint {
	if days <= 0 {
		days = 90
	}
	pid := projectID
	return s.uptimeTrend(ctx, scope{projectID: &pid}, days)
}

// SLAStatus classifies an uptime percentage against a target: meeting at/above target,
// at_risk within 0.5pp below, breached otherwise.
func SLAStatus(uptimePct, target float64) string {
	switch {
	case uptimePct >= target:
		return models.SLAMeeting
	case uptimePct >= target-0.5:
		return models.SLAAtRisk
	default:
		return models.SLABreached
	}
}

// ───────────────────────── SLA configuration ─────────────────────────

// GetSLA returns the configured SLA for an environment, or a default (99.9% / 30d) row when
// none is set yet. The returned row's ID is uuid.Nil when defaulted.
func (s *AnalyticsService) GetSLA(ctx context.Context, environmentID uuid.UUID) (*models.ServiceSLA, error) {
	var sla models.ServiceSLA
	err := s.db.Pool.QueryRow(ctx, `
		SELECT id, org_id, project_id, environment_id, target_uptime_pct, measurement_window_days, created_at, updated_at
		FROM service_slas WHERE environment_id = $1`, environmentID).Scan(
		&sla.ID, &sla.OrgID, &sla.ProjectID, &sla.EnvironmentID,
		&sla.TargetUptimePct, &sla.MeasurementWindowDays, &sla.CreatedAt, &sla.UpdatedAt)
	if err != nil {
		// Default (unconfigured) — resolve org/project for display.
		sla = models.ServiceSLA{EnvironmentID: environmentID, TargetUptimePct: defaultSLATarget, MeasurementWindowDays: 30}
		_ = s.db.Pool.QueryRow(ctx,
			`SELECT p.org_id, p.id FROM environments e JOIN projects p ON p.id = e.project_id WHERE e.id = $1`,
			environmentID).Scan(&sla.OrgID, &sla.ProjectID)
	}
	return &sla, nil
}

// UpsertSLA sets the SLA target (and optional window) for an environment. org_id/project_id
// are derived from the environment.
func (s *AnalyticsService) UpsertSLA(ctx context.Context, environmentID uuid.UUID, targetUptimePct float64, windowDays int) error {
	var orgID, projectID uuid.UUID
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT p.org_id, p.id FROM environments e JOIN projects p ON p.id = e.project_id WHERE e.id = $1`,
		environmentID).Scan(&orgID, &projectID); err != nil {
		return fmt.Errorf("resolve environment: %w", err)
	}
	if windowDays <= 0 {
		windowDays = 30
	}
	_, err := s.db.Pool.Exec(ctx, `
		INSERT INTO service_slas (org_id, project_id, environment_id, target_uptime_pct, measurement_window_days)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (environment_id) DO UPDATE SET
		    target_uptime_pct = EXCLUDED.target_uptime_pct,
		    measurement_window_days = EXCLUDED.measurement_window_days,
		    updated_at = NOW()`,
		orgID, projectID, environmentID, targetUptimePct, windowDays)
	return err
}

// mondayOf returns the Monday (00:00 UTC) of t's ISO week.
func mondayOf(t time.Time) time.Time {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	offset := (int(t.Weekday()) + 6) % 7 // Mon=0 … Sun=6
	return t.AddDate(0, 0, -offset)
}

// marshalJSON is a tiny helper so the monthly report can store structured metrics.
func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
