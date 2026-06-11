// handlers.go exposes the alert lifecycle over HTTP: list alerts for a project,
// snooze an alert type, and manually resolve an alert. All routes sit under
// /projects/:id behind RequireAuth + RequireProjectOwnership.
package monitor

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HandleListAlerts — GET /projects/:id/alerts?status=open|resolved|all&limit=50
func (a *AlertEngine) HandleListAlerts(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	status := c.DefaultQuery("status", models.AlertStatusOpen)
	limit := 50
	if n, err := strconv.Atoi(c.Query("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}

	query := `SELECT id, project_id, environment_id, alert_type, severity, title, summary,
	                 status, triggered_at, resolved_at, snoozed_until, created_at
	          FROM alerts WHERE project_id = $1`
	args := []any{projectID}
	if status != "all" {
		if status != models.AlertStatusOpen && status != models.AlertStatusResolved && status != models.AlertStatusSnoozed {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status must be open, resolved, snoozed, or all"})
			return
		}
		query += ` AND status = $2`
		args = append(args, status)
	}
	query += ` ORDER BY triggered_at DESC LIMIT ` + strconv.Itoa(limit)

	rows, err := a.db.Pool.Query(c.Request.Context(), query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list alerts"})
		return
	}
	defer rows.Close()

	alerts := []models.Alert{}
	for rows.Next() {
		var al models.Alert
		if err := rows.Scan(&al.ID, &al.ProjectID, &al.EnvironmentID, &al.AlertType, &al.Severity,
			&al.Title, &al.Summary, &al.Status, &al.TriggeredAt, &al.ResolvedAt, &al.SnoozedUntil,
			&al.CreatedAt); err != nil {
			continue
		}
		alerts = append(alerts, al)
	}
	c.JSON(http.StatusOK, alerts)
}

// HandleSnooze — POST /projects/:id/alerts/:alertId/snooze {duration_minutes}
// Snoozes the alert and records an alert preference so the engine suppresses
// the same alert type for this environment until the snooze expires.
func (a *AlertEngine) HandleSnooze(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	alertID, err := uuid.Parse(c.Param("alertId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alert id"})
		return
	}

	var body struct {
		DurationMinutes int `json:"duration_minutes"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.DurationMinutes <= 0 {
		body.DurationMinutes = 60
	}
	if body.DurationMinutes > 7*24*60 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "snooze duration too long (max 7 days)"})
		return
	}
	until := time.Now().Add(time.Duration(body.DurationMinutes) * time.Minute)

	var envID *uuid.UUID
	var alertType string
	err = a.db.Pool.QueryRow(c.Request.Context(), `
		UPDATE alerts SET status = $1, snoozed_until = $2
		WHERE id = $3 AND project_id = $4
		RETURNING environment_id, alert_type`,
		models.AlertStatusSnoozed, until, alertID, projectID,
	).Scan(&envID, &alertType)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
		return
	}

	// Persist the preference so re-fired alerts of this type stay quiet too.
	_, err = a.db.Pool.Exec(c.Request.Context(), `
		INSERT INTO alert_preferences (project_id, environment_id, alert_type, snoozed_until)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id, environment_id, alert_type)
		DO UPDATE SET snoozed_until = EXCLUDED.snoozed_until`,
		projectID, envID, alertType, until,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save snooze preference"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "alert snoozed", "snoozed_until": until})
}

// HandleResolve — POST /projects/:id/alerts/:alertId/resolve
func (a *AlertEngine) HandleResolve(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	alertID, err := uuid.Parse(c.Param("alertId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alert id"})
		return
	}

	var alertType string
	err = a.db.Pool.QueryRow(c.Request.Context(), `
		UPDATE alerts SET status = $1, resolved_at = NOW()
		WHERE id = $2 AND project_id = $3 AND status <> $1
		RETURNING alert_type`,
		models.AlertStatusResolved, alertID, projectID,
	).Scan(&alertType)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert not found or already resolved"})
		return
	}

	payload, _ := json.Marshal(map[string]string{"id": alertID.String(), "alert_type": alertType})
	a.hub.Broadcast(projectID.String(), ws.Message{Type: "alert_resolved", Payload: string(payload)})

	c.JSON(http.StatusOK, gin.H{"message": "alert resolved"})
}
