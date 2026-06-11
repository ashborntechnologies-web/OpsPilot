package diagnosis

import (
	"context"
	"net/http"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HandleSubmitFeedback records a user's rating of a diagnosis.
// POST /projects/:id/deployments/:deployId/diagnose/feedback
// Body: {"incident_id": "...", "rating": "helpful", "fixed_issue": true, "notes": "..."}
// incident_id is optional — when absent, the most recent incident for the
// deployment is rated. One rating per user per incident (re-submitting updates it).
func (s *Service) HandleSubmitFeedback(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	deploymentID, err := uuid.Parse(c.Param("deployId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deployment id"})
		return
	}
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	var req struct {
		IncidentID string `json:"incident_id"`
		Rating     string `json:"rating" binding:"required"`
		FixedIssue bool   `json:"fixed_issue"`
		Notes      string `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	switch req.Rating {
	case models.RatingHelpful, models.RatingNotHelpful, models.RatingPartiallyHelpful:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "rating must be helpful, not_helpful, or partially_helpful"})
		return
	}
	if len(req.Notes) > 4000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "notes too long (max 4000 characters)"})
		return
	}

	// Resolve the incident — explicit ID, or the deployment's most recent one.
	// Both paths are scoped to the project so a foreign incident cannot be rated.
	var incidentID uuid.UUID
	if req.IncidentID != "" {
		id, err := uuid.Parse(req.IncidentID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid incident_id"})
			return
		}
		err = s.db.Pool.QueryRow(c.Request.Context(),
			`SELECT id FROM incidents WHERE id = $1 AND project_id = $2 AND deployment_id = $3`,
			id, projectID, deploymentID,
		).Scan(&incidentID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
			return
		}
	} else {
		err = s.db.Pool.QueryRow(c.Request.Context(),
			`SELECT id FROM incidents WHERE project_id = $1 AND deployment_id = $2
			 ORDER BY created_at DESC LIMIT 1`,
			projectID, deploymentID,
		).Scan(&incidentID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no diagnosis found for this deployment — run a diagnosis first"})
			return
		}
	}

	var fb models.DiagnosisFeedback
	err = s.db.Pool.QueryRow(c.Request.Context(),
		`INSERT INTO diagnosis_feedback (incident_id, project_id, user_id, rating, fixed_issue, notes)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (incident_id, user_id)
		 DO UPDATE SET rating = EXCLUDED.rating, fixed_issue = EXCLUDED.fixed_issue, notes = EXCLUDED.notes
		 RETURNING id, incident_id, project_id, user_id, rating, fixed_issue, notes, created_at`,
		incidentID, projectID, userID, req.Rating, req.FixedIssue, req.Notes,
	).Scan(&fb.ID, &fb.IncidentID, &fb.ProjectID, &fb.UserID, &fb.Rating, &fb.FixedIssue, &fb.Notes, &fb.CreatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save feedback"})
		return
	}

	// Positive, verified feedback becomes long-term project memory.
	if s.memory != nil && fb.Rating == models.RatingHelpful && fb.FixedIssue {
		go s.memory.RecordDiagnosisFeedback(context.Background(), projectID, incidentID)
	}

	c.JSON(http.StatusCreated, gin.H{"feedback": fb, "score": fb.RatingScore()})
}

// HandleFeedbackSummary aggregates diagnosis quality for a project.
// GET /projects/:id/diagnose/feedback-summary → {total, helpful_pct, fix_rate}
func (s *Service) HandleFeedbackSummary(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	var total, helpful, fixed int
	err = s.db.Pool.QueryRow(c.Request.Context(),
		`SELECT COUNT(*),
		        COUNT(*) FILTER (WHERE rating = 'helpful'),
		        COUNT(*) FILTER (WHERE fixed_issue)
		 FROM diagnosis_feedback WHERE project_id = $1`,
		projectID,
	).Scan(&total, &helpful, &fixed)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to compute feedback summary"})
		return
	}

	helpfulPct, fixRate := 0.0, 0.0
	if total > 0 {
		helpfulPct = float64(helpful) / float64(total) * 100
		fixRate = float64(fixed) / float64(total) * 100
	}
	c.JSON(http.StatusOK, gin.H{
		"total":       total,
		"helpful_pct": helpfulPct,
		"fix_rate":    fixRate,
	})
}
