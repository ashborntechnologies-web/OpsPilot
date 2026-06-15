package trust

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HandleGetTrust returns an environment's trust level + boundaries.
// GET /projects/:id/environments/:envId/trust — any member (project group loaded membership).
func (s *Service) HandleGetTrust(c *gin.Context) {
	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
		return
	}
	var trustLevel string
	var boundsRaw []byte
	if err := s.db.Pool.QueryRow(c.Request.Context(),
		`SELECT trust_level, autonomous_boundaries FROM environments WHERE id = $1`, envID,
	).Scan(&trustLevel, &boundsRaw); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "environment not found"})
		return
	}
	var bounds *models.AutonomousBoundaries
	if len(boundsRaw) > 0 {
		var b models.AutonomousBoundaries
		if json.Unmarshal(boundsRaw, &b) == nil {
			bounds = &b
		}
	}
	c.JSON(http.StatusOK, gin.H{"trust_level": trustLevel, "autonomous_boundaries": bounds})
}

// HandleUpdateTrust sets an environment's trust level + boundaries. Admin only
// (RequireRole(admin) is applied on the route).
// PATCH /projects/:id/environments/:envId/trust
func (s *Service) HandleUpdateTrust(c *gin.Context) {
	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
		return
	}
	var req struct {
		TrustLevel           *string                      `json:"trust_level"`
		AutonomousBoundaries *models.AutonomousBoundaries `json:"autonomous_boundaries"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.TrustLevel != nil {
		switch *req.TrustLevel {
		case models.TrustSuggest, models.TrustSupervised, models.TrustAutonomous:
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "trust_level must be suggest, supervised, or autonomous"})
			return
		}
	}
	var boundsJSON []byte
	if req.AutonomousBoundaries != nil {
		boundsJSON, _ = json.Marshal(req.AutonomousBoundaries)
	}
	_, err = s.db.Pool.Exec(c.Request.Context(), `
		UPDATE environments SET
		    trust_level = COALESCE($2, trust_level),
		    autonomous_boundaries = COALESCE($3, autonomous_boundaries),
		    updated_at = NOW()
		WHERE id = $1`,
		envID, req.TrustLevel, boundsJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update trust settings"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "trust settings updated"})
}

const aiActionSelect = `
	SELECT a.id, a.org_id, a.project_id, a.environment_id, a.incident_id,
	       a.proposed_by_type, a.proposed_by_user_id, a.action_type, a.parameters,
	       a.confidence_score, a.rationale, a.status, a.approved_by, a.approval_required,
	       a.proposed_at, a.decided_at, a.executed_at, a.result,
	       COALESCE(e.name,''), COALESCE(p.name,''), COALESCE(pu.email,''), COALESCE(au.email,'')
	FROM ai_actions a
	LEFT JOIN environments e ON e.id = a.environment_id
	LEFT JOIN projects p     ON p.id = a.project_id
	LEFT JOIN users pu       ON pu.id = a.proposed_by_user_id
	LEFT JOIN users au       ON au.id = a.approved_by`

// HandleListOrgActions lists actions across an org (default: pending approval).
// GET /orgs/:orgId/actions?status=pending — any member.
func (s *Service) HandleListOrgActions(c *gin.Context) {
	orgID, ok := middleware.GetOrgID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "organization not resolved"})
		return
	}
	query := aiActionSelect + ` WHERE a.org_id = $1`
	args := []any{orgID}
	if status := c.Query("status"); status != "" && status != "all" {
		// "pending" is shorthand for the stored "pending_approval".
		if status == "pending" {
			status = models.ActionStatusPending
		}
		args = append(args, status)
		query += ` AND a.status = $2`
	}
	query += ` ORDER BY a.proposed_at DESC LIMIT 100`
	s.writeActionList(c, query, args...)
}

// HandleListProjectActions returns action history for a project.
// GET /projects/:id/actions?limit=50 — any member.
func (s *Service) HandleListProjectActions(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	limit := 50
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	s.writeActionList(c, aiActionSelect+` WHERE a.project_id = $1 ORDER BY a.proposed_at DESC LIMIT `+strconv.Itoa(limit), projectID)
}

// HandleApprove approves a pending action. POST /actions/:actionId/approve — engineer+
// (enforced in ApproveAction against the action's org).
func (s *Service) HandleApprove(c *gin.Context) {
	s.decide(c, true)
}

// HandleReject rejects a pending action. POST /actions/:actionId/reject — engineer+.
func (s *Service) HandleReject(c *gin.Context) {
	s.decide(c, false)
}

func (s *Service) decide(c *gin.Context, approve bool) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}
	actionID, err := uuid.Parse(c.Param("actionId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid action id"})
		return
	}
	if approve {
		err = s.ApproveAction(c.Request.Context(), actionID, userID)
	} else {
		err = s.RejectAction(c.Request.Context(), actionID, userID)
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	verb := "approved"
	if !approve {
		verb = "rejected"
	}
	c.JSON(http.StatusOK, gin.H{"message": "action " + verb})
}

func (s *Service) writeActionList(c *gin.Context, query string, args ...any) {
	rows, err := s.db.Pool.Query(c.Request.Context(), query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list actions"})
		return
	}
	defer rows.Close()
	out := []models.AIAction{}
	for rows.Next() {
		var a models.AIAction
		var paramsRaw, resultRaw []byte
		if err := rows.Scan(&a.ID, &a.OrgID, &a.ProjectID, &a.EnvironmentID, &a.IncidentID,
			&a.ProposedByType, &a.ProposedByUserID, &a.ActionType, &paramsRaw,
			&a.ConfidenceScore, &a.Rationale, &a.Status, &a.ApprovedBy, &a.ApprovalRequired,
			&a.ProposedAt, &a.DecidedAt, &a.ExecutedAt, &resultRaw,
			&a.EnvironmentName, &a.ProjectName, &a.ProposedByName, &a.ApprovedByName); err != nil {
			continue
		}
		_ = json.Unmarshal(paramsRaw, &a.Parameters)
		if len(resultRaw) > 0 {
			_ = json.Unmarshal(resultRaw, &a.Result)
		}
		out = append(out, a)
	}
	c.JSON(http.StatusOK, out)
}
