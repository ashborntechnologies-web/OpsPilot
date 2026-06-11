// Package users exposes account-level endpoints: the current user's plan and
// usage (GET /users/me) and notification preferences (PATCH /users/me/notifications).
package users

import (
	"net/http"

	"github.com/ashborntechnologies-web/OpsPilot/internal/billing"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
)

type Service struct {
	db      *models.DB
	billing *billing.Service
}

func NewService(db *models.DB, billingSvc *billing.Service) *Service {
	return &Service{db: db, billing: billingSvc}
}

// HandleGetMe — GET /users/me — plan, AI usage, project counts, and
// notification preferences for the authenticated user.
func (s *Service) HandleGetMe(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	usage, err := s.billing.GetUsage(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load usage"})
		return
	}

	var notifEnabled, notifFailed, notifSucceeded, notifAlert bool
	var email string
	err = s.db.Pool.QueryRow(c.Request.Context(), `
		SELECT email, notifications_enabled, notify_deploy_failed, notify_deploy_succeeded, notify_alert_fired
		FROM users WHERE id = $1`, userID,
	).Scan(&email, &notifEnabled, &notifFailed, &notifSucceeded, &notifAlert)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"email":                 email,
		"plan":                  usage.Plan,
		"ai_actions_this_month": usage.AIActionsThisMonth,
		"ai_actions_limit":      usage.AIActionsLimit,
		"projects_count":        usage.ProjectsCount,
		"projects_limit":        usage.ProjectsLimit,
		"notifications": gin.H{
			"enabled":          notifEnabled,
			"deploy_failed":    notifFailed,
			"deploy_succeeded": notifSucceeded,
			"alert_fired":      notifAlert,
		},
	})
}

// HandleUpdateNotifications — PATCH /users/me/notifications
// Body: any subset of {enabled, deploy_failed, deploy_succeeded, alert_fired}.
func (s *Service) HandleUpdateNotifications(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	var req struct {
		Enabled         *bool `json:"enabled"`
		DeployFailed    *bool `json:"deploy_failed"`
		DeploySucceeded *bool `json:"deploy_succeeded"`
		AlertFired      *bool `json:"alert_fired"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	_, err := s.db.Pool.Exec(c.Request.Context(), `
		UPDATE users SET
			notifications_enabled   = COALESCE($1, notifications_enabled),
			notify_deploy_failed    = COALESCE($2, notify_deploy_failed),
			notify_deploy_succeeded = COALESCE($3, notify_deploy_succeeded),
			notify_alert_fired      = COALESCE($4, notify_alert_fired),
			updated_at = NOW()
		WHERE id = $5`,
		req.Enabled, req.DeployFailed, req.DeploySucceeded, req.AlertFired, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update preferences"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "notification preferences updated"})
}
