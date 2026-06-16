package summary

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
)

// HandleListSummaries lists past summaries for an org. GET /orgs/:orgId/summaries?limit=30 — member.
func (s *Service) HandleListSummaries(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	limit := 30
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v <= 365 {
		limit = v
	}
	rows, err := s.db.Pool.Query(c.Request.Context(), `
		SELECT id, org_id, summary_date::text, content_markdown, content_json,
		       generated_at, delivered_slack, delivered_email, created_at
		FROM daily_summaries WHERE org_id = $1 AND is_monthly = false
		ORDER BY summary_date DESC LIMIT $2`, orgID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list summaries"})
		return
	}
	defer rows.Close()
	c.JSON(http.StatusOK, scanSummaries(rows))
}

// HandleLatestSummary returns the most recent summary. GET /orgs/:orgId/summaries/latest — member.
func (s *Service) HandleLatestSummary(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	rows, err := s.db.Pool.Query(c.Request.Context(), `
		SELECT id, org_id, summary_date::text, content_markdown, content_json,
		       generated_at, delivered_slack, delivered_email, created_at
		FROM daily_summaries WHERE org_id = $1 AND is_monthly = false
		ORDER BY summary_date DESC LIMIT 1`, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load summary"})
		return
	}
	defer rows.Close()
	list := scanSummaries(rows)
	if len(list) == 0 {
		c.JSON(http.StatusOK, gin.H{"summary": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": list[0]})
}

// HandleGenerateNow generates + delivers today's summary on demand (testing).
// POST /orgs/:orgId/summaries/generate — admin.
func (s *Service) HandleGenerateNow(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	sum, err := s.GenerateAndDeliver(c.Request.Context(), orgID, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sum)
}

// HandleUpdateConfig updates the org's summary schedule. PATCH /orgs/:orgId/summary-config — admin.
func (s *Service) HandleUpdateConfig(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	var req struct {
		SummaryTime     *string `json:"summary_time"`     // "HH:MM" or "HH:MM:SS"
		SummaryTimezone *string `json:"summary_timezone"` // IANA name
		SummaryEnabled  *bool   `json:"summary_enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.SummaryTimezone != nil {
		if _, err := time.LoadLocation(*req.SummaryTimezone); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid timezone (must be an IANA name like America/New_York)"})
			return
		}
	}
	_, err := s.db.Pool.Exec(c.Request.Context(), `
		UPDATE organizations SET
		    summary_time     = COALESCE($2::time, summary_time),
		    summary_timezone = COALESCE($3, summary_timezone),
		    summary_enabled  = COALESCE($4, summary_enabled),
		    updated_at = NOW()
		WHERE id = $1`,
		orgID, req.SummaryTime, req.SummaryTimezone, req.SummaryEnabled)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update summary config"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "summary config updated"})
}

func scanSummaries(rows interface {
	Next() bool
	Scan(...any) error
}) []models.DailySummaryRecord {
	out := []models.DailySummaryRecord{}
	for rows.Next() {
		var r models.DailySummaryRecord
		var raw []byte
		if rows.Scan(&r.ID, &r.OrgID, &r.SummaryDate, &r.ContentMarkdown, &raw,
			&r.GeneratedAt, &r.DeliveredSlack, &r.DeliveredEmail, &r.CreatedAt) != nil {
			continue
		}
		_ = json.Unmarshal(raw, &r.ContentJSON)
		out = append(out, r)
	}
	return out
}
