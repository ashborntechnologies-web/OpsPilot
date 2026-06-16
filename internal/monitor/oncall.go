package monitor

import (
	"net/http"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
)

var validWeekdays = map[string]bool{
	"monday": true, "tuesday": true, "wednesday": true, "thursday": true,
	"friday": true, "saturday": true, "sunday": true,
}

// HandleGetOncallSchedule returns the org's on-call schedule, or sensible defaults when
// none is configured. GET /orgs/:orgId/oncall-schedule — member.
func (a *AlertEngine) HandleGetOncallSchedule(c *gin.Context) {
	orgID, ok := middleware.GetOrgID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "organization not resolved"})
		return
	}
	var s models.OncallSchedule
	err := a.db.Pool.QueryRow(c.Request.Context(), `
		SELECT id, org_id, timezone, to_char(quiet_hours_start, 'HH24:MI'),
		       to_char(quiet_hours_end, 'HH24:MI'), quiet_days, escalation_after_minutes, created_at, updated_at
		FROM oncall_schedules WHERE org_id = $1`, orgID).Scan(
		&s.ID, &s.OrgID, &s.Timezone, &s.QuietHoursStart, &s.QuietHoursEnd,
		&s.QuietDays, &s.EscalationAfterMinutes, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		// Unconfigured — return defaults so the settings form has something to render.
		c.JSON(http.StatusOK, models.OncallSchedule{
			OrgID: orgID, Timezone: "UTC", QuietHoursStart: "22:00", QuietHoursEnd: "08:00",
			QuietDays: []string{}, EscalationAfterMinutes: 15,
		})
		return
	}
	if s.QuietDays == nil {
		s.QuietDays = []string{}
	}
	c.JSON(http.StatusOK, s)
}

// HandlePutOncallSchedule creates or replaces the org's on-call schedule.
// PUT /orgs/:orgId/oncall-schedule — admin.
func (a *AlertEngine) HandlePutOncallSchedule(c *gin.Context) {
	orgID, ok := middleware.GetOrgID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "organization not resolved"})
		return
	}
	var req struct {
		Timezone               string   `json:"timezone"`
		QuietHoursStart        string   `json:"quiet_hours_start"` // "HH:MM"
		QuietHoursEnd          string   `json:"quiet_hours_end"`   // "HH:MM"
		QuietDays              []string `json:"quiet_days"`
		EscalationAfterMinutes int      `json:"escalation_after_minutes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid timezone (must be an IANA name like America/New_York)"})
		return
	}
	if _, ok := parseHHMM(req.QuietHoursStart); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quiet_hours_start must be HH:MM"})
		return
	}
	if _, ok := parseHHMM(req.QuietHoursEnd); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quiet_hours_end must be HH:MM"})
		return
	}
	// Normalize + validate quiet days (lowercase weekday names).
	days := make([]string, 0, len(req.QuietDays))
	for _, d := range req.QuietDays {
		d = strings.ToLower(strings.TrimSpace(d))
		if !validWeekdays[d] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "quiet_days must be weekday names (e.g. saturday)"})
			return
		}
		days = append(days, d)
	}
	if req.EscalationAfterMinutes <= 0 {
		req.EscalationAfterMinutes = 15
	}

	_, err := a.db.Pool.Exec(c.Request.Context(), `
		INSERT INTO oncall_schedules
		    (org_id, timezone, quiet_hours_start, quiet_hours_end, quiet_days, escalation_after_minutes)
		VALUES ($1, $2, $3::time, $4::time, $5, $6)
		ON CONFLICT (org_id) DO UPDATE SET
		    timezone = EXCLUDED.timezone,
		    quiet_hours_start = EXCLUDED.quiet_hours_start,
		    quiet_hours_end = EXCLUDED.quiet_hours_end,
		    quiet_days = EXCLUDED.quiet_days,
		    escalation_after_minutes = EXCLUDED.escalation_after_minutes,
		    updated_at = NOW()`,
		orgID, req.Timezone, req.QuietHoursStart, req.QuietHoursEnd, days, req.EscalationAfterMinutes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save on-call schedule"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "on-call schedule saved"})
}
