package analytics

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// queryDays parses ?days= with a default and clamp.
func queryDays(c *gin.Context, def, max int) int {
	if v, err := strconv.Atoi(c.Query("days")); err == nil && v > 0 {
		if v > max {
			return max
		}
		return v
	}
	return def
}

// HandleOrgAnalytics returns org-wide reliability metrics + a per-project breakdown.
// GET /orgs/:orgId/analytics?days=30 — member.
func (s *AnalyticsService) HandleOrgAnalytics(c *gin.Context) {
	orgID, ok := middleware.GetOrgID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "organization not resolved"})
		return
	}
	days := queryDays(c, 30, 365)
	metrics, err := s.GetReliabilityMetrics(c.Request.Context(), orgID, nil, days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to compute metrics"})
		return
	}
	breakdown, _ := s.GetProjectBreakdown(c.Request.Context(), orgID, days)
	c.JSON(http.StatusOK, gin.H{"metrics": metrics, "breakdown": breakdown})
}

// HandleProjectAnalytics returns reliability metrics for a single project.
// GET /projects/:id/analytics?days=30 — member (project tier).
func (s *AnalyticsService) HandleProjectAnalytics(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	days := queryDays(c, 30, 365)
	metrics, err := s.GetReliabilityMetrics(c.Request.Context(), orgID, &projectID, days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to compute metrics"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"metrics": metrics})
}

// HandleProjectUptime returns daily uptime points for charting.
// GET /projects/:id/uptime?days=90 — member (project tier).
func (s *AnalyticsService) HandleProjectUptime(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	days := queryDays(c, 90, 365)
	points := s.GetUptimeHistory(c.Request.Context(), projectID, days)
	c.JSON(http.StatusOK, gin.H{"uptime": points, "days": days})
}

// HandleGetSLA returns the SLA config for an environment (default if unset).
// GET /projects/:id/environments/:envId/sla — member (project tier).
func (s *AnalyticsService) HandleGetSLA(c *gin.Context) {
	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
		return
	}
	sla, err := s.GetSLA(c.Request.Context(), envID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load SLA"})
		return
	}
	c.JSON(http.StatusOK, sla)
}

// HandleSetSLA upserts the SLA target for an environment.
// PUT /projects/:id/environments/:envId/sla — engineer+ (project tier).
func (s *AnalyticsService) HandleSetSLA(c *gin.Context) {
	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
		return
	}
	var req struct {
		TargetUptimePct       float64 `json:"target_uptime_pct"`
		MeasurementWindowDays int     `json:"measurement_window_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.TargetUptimePct <= 0 || req.TargetUptimePct > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_uptime_pct must be between 0 and 100"})
		return
	}
	if err := s.UpsertSLA(c.Request.Context(), envID, req.TargetUptimePct, req.MeasurementWindowDays); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save SLA"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "SLA target saved"})
}

// HandleListReports lists stored monthly reports for an org.
// GET /orgs/:orgId/reports — member.
func (s *AnalyticsService) HandleListReports(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	rows, err := s.db.Pool.Query(c.Request.Context(), `
		SELECT id, org_id, summary_date::text, content_markdown, content_json,
		       generated_at, delivered_slack, delivered_email, is_monthly, created_at
		FROM daily_summaries WHERE org_id = $1 AND is_monthly = true
		ORDER BY summary_date DESC LIMIT 36`, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list reports"})
		return
	}
	defer rows.Close()
	out := []models.DailySummaryRecord{}
	for rows.Next() {
		var r models.DailySummaryRecord
		var raw []byte
		if rows.Scan(&r.ID, &r.OrgID, &r.SummaryDate, &r.ContentMarkdown, &raw,
			&r.GeneratedAt, &r.DeliveredSlack, &r.DeliveredEmail, &r.IsMonthly, &r.CreatedAt) != nil {
			continue
		}
		_ = json.Unmarshal(raw, &r.ContentJSON)
		out = append(out, r)
	}
	c.JSON(http.StatusOK, out)
}

// HandleGenerateReport generates the monthly report on demand. An optional ?month=YYYY-MM
// targets a specific month (defaults to the current month).
// POST /orgs/:orgId/reports/generate — admin.
func (s *AnalyticsService) HandleGenerateReport(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	month := time.Now().UTC()
	if v := c.Query("month"); v != "" {
		if t, err := time.Parse("2006-01", v); err == nil {
			month = t
		}
	}
	rep, err := s.GenerateMonthlyReport(c.Request.Context(), orgID, month)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rep)
}
