package incidents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// incidentListColumns + scan order shared by the org and project list queries.
const incidentListSelect = `
	SELECT i.id, i.project_id, i.org_id, i.deployment_id, i.environment_id, i.trigger,
	       i.title, i.status, i.severity, i.acknowledged_by, i.acknowledged_at,
	       i.resolved_by, i.resolved_at, i.created_at,
	       COALESCE(e.name, ''), COALESCE(p.name, ''), COALESCE(au.email, '')
	FROM incidents i
	LEFT JOIN environments e ON e.id = i.environment_id
	LEFT JOIN projects p     ON p.id = i.project_id
	LEFT JOIN users au       ON au.id = i.acknowledged_by`

// open incidents first, then by severity (error > warn > info), then most recent.
const incidentListOrder = `
	ORDER BY (i.status = 'resolved'),
	         CASE i.severity WHEN 'error' THEN 0 WHEN 'warn' THEN 1 ELSE 2 END,
	         i.created_at DESC
	LIMIT $2 OFFSET $3`

func paginate(c *gin.Context) (limit, offset int) {
	limit, offset = 50, 0
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v >= 0 {
		offset = v
	}
	return
}

// HandleListOrgIncidents lists incidents across an org. GET /orgs/:orgId/incidents — any member.
func (s *Service) HandleListOrgIncidents(c *gin.Context) {
	orgID, ok := middleware.GetOrgID(c) // set by RequireOrgMembership
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "organization not resolved"})
		return
	}
	limit, offset := paginate(c)
	rows, err := s.db.Pool.Query(c.Request.Context(),
		incidentListSelect+` WHERE i.org_id = $1`+incidentListOrder, orgID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list incidents"})
		return
	}
	s.writeIncidentList(c, rows)
}

// HandleListProjectIncidents lists a project's incidents. GET /projects/:id/incidents — any member.
func (s *Service) HandleListProjectIncidents(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	limit, offset := paginate(c)
	rows, err := s.db.Pool.Query(c.Request.Context(),
		incidentListSelect+` WHERE i.project_id = $1`+incidentListOrder, projectID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list incidents"})
		return
	}
	s.writeIncidentList(c, rows)
}

func (s *Service) writeIncidentList(c *gin.Context, rows pgx.Rows) {
	defer rows.Close()
	out := []models.Incident{}
	for rows.Next() {
		var i models.Incident
		if err := rows.Scan(&i.ID, &i.ProjectID, &i.OrgID, &i.DeploymentID, &i.EnvironmentID, &i.Trigger,
			&i.Title, &i.Status, &i.Severity, &i.AcknowledgedBy, &i.AcknowledgedAt,
			&i.ResolvedBy, &i.ResolvedAt, &i.CreatedAt,
			&i.EnvironmentName, &i.ProjectName, &i.AcknowledgedByName); err != nil {
			continue
		}
		out = append(out, i)
	}
	c.JSON(http.StatusOK, out)
}

// HandleGetIncident returns the full incident with timeline + actions.
// GET /incidents/:incidentId — any member of the incident's org.
func (s *Service) HandleGetIncident(c *gin.Context) {
	userID, incidentID, ok := s.requireIncidentRole(c, models.RoleViewer)
	if !ok {
		return
	}
	_ = userID
	ctx := c.Request.Context()

	var i models.Incident
	err := s.db.Pool.QueryRow(ctx, `
		SELECT i.id, i.project_id, i.org_id, i.deployment_id, i.environment_id, i.trigger,
		       i.root_cause, i.resolution, i.title, i.status, i.severity,
		       i.acknowledged_by, i.acknowledged_at, i.resolved_by, i.resolved_at, i.postmortem, i.created_at,
		       COALESCE(e.name, ''), COALESCE(p.name, ''), COALESCE(au.email, '')
		FROM incidents i
		LEFT JOIN environments e ON e.id = i.environment_id
		LEFT JOIN projects p     ON p.id = i.project_id
		LEFT JOIN users au       ON au.id = i.acknowledged_by
		WHERE i.id = $1`, incidentID,
	).Scan(&i.ID, &i.ProjectID, &i.OrgID, &i.DeploymentID, &i.EnvironmentID, &i.Trigger,
		&i.RootCause, &i.Resolution, &i.Title, &i.Status, &i.Severity,
		&i.AcknowledgedBy, &i.AcknowledgedAt, &i.ResolvedBy, &i.ResolvedAt, &i.Postmortem, &i.CreatedAt,
		&i.EnvironmentName, &i.ProjectName, &i.AcknowledgedByName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"incident": i,
		"timeline": s.loadTimeline(ctx, incidentID),
		"actions":  s.loadActions(ctx, incidentID),
	})
}

func (s *Service) loadTimeline(ctx context.Context, incidentID uuid.UUID) []models.IncidentTimelineEntry {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT t.id, t.incident_id, t.author_type, t.author_id, t.content, t.entry_type, t.metadata, t.created_at,
		       COALESCE(u.email, '')
		FROM incident_timeline t LEFT JOIN users u ON u.id = t.author_id
		WHERE t.incident_id = $1 ORDER BY t.created_at ASC`, incidentID)
	if err != nil {
		return []models.IncidentTimelineEntry{}
	}
	defer rows.Close()
	out := []models.IncidentTimelineEntry{}
	for rows.Next() {
		var e models.IncidentTimelineEntry
		var meta []byte
		if rows.Scan(&e.ID, &e.IncidentID, &e.AuthorType, &e.AuthorID, &e.Content, &e.EntryType, &meta, &e.CreatedAt, &e.AuthorName) != nil {
			continue
		}
		_ = json.Unmarshal(meta, &e.Metadata)
		out = append(out, e)
	}
	return out
}

func (s *Service) loadActions(ctx context.Context, incidentID uuid.UUID) []models.IncidentAction {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT a.id, a.incident_id, a.proposed_by, a.action_type, a.parameters, a.status,
		       a.approved_by, a.executed_at, a.created_at, COALESCE(u.email, '')
		FROM incident_actions a LEFT JOIN users u ON u.id = a.approved_by
		WHERE a.incident_id = $1 ORDER BY a.created_at ASC`, incidentID)
	if err != nil {
		return []models.IncidentAction{}
	}
	defer rows.Close()
	out := []models.IncidentAction{}
	for rows.Next() {
		var a models.IncidentAction
		var params []byte
		if rows.Scan(&a.ID, &a.IncidentID, &a.ProposedBy, &a.ActionType, &params, &a.Status,
			&a.ApprovedBy, &a.ExecutedAt, &a.CreatedAt, &a.ApprovedByName) != nil {
			continue
		}
		_ = json.Unmarshal(params, &a.Parameters)
		out = append(out, a)
	}
	return out
}

// HandlePostTimeline posts a human update to the timeline. POST /incidents/:id/timeline — engineer+.
func (s *Service) HandlePostTimeline(c *gin.Context) {
	userID, incidentID, ok := s.requireIncidentRole(c, models.RoleEngineer)
	if !ok {
		return
	}
	var req struct {
		Content   string `json:"content" binding:"required"`
		EntryType string `json:"entry_type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content is required"})
		return
	}
	entryType := req.EntryType
	if entryType == "" {
		entryType = models.IncidentEntryUpdate
	}
	entry, err := s.postTimelineEntry(c.Request.Context(), incidentID, models.IncidentAuthorHuman, &userID, req.Content, entryType, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to post update"})
		return
	}
	c.JSON(http.StatusCreated, entry)
}

// HandleAcknowledge marks an incident as investigating. POST /incidents/:id/acknowledge — engineer+.
func (s *Service) HandleAcknowledge(c *gin.Context) {
	userID, incidentID, ok := s.requireIncidentRole(c, models.RoleEngineer)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	_, err := s.db.Pool.Exec(ctx, `
		UPDATE incidents
		SET status = CASE WHEN status = 'open' THEN 'investigating' ELSE status END,
		    acknowledged_by = COALESCE(acknowledged_by, $2),
		    acknowledged_at = COALESCE(acknowledged_at, NOW())
		WHERE id = $1 AND status <> 'resolved'`, incidentID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to acknowledge"})
		return
	}
	_, _ = s.postTimelineEntry(ctx, incidentID, models.IncidentAuthorHuman, &userID,
		"Acknowledged — investigating.", models.IncidentEntryUpdate, nil)
	s.broadcast(incidentID, "incident_update", gin.H{"status": models.IncidentStatusInvestigating})
	c.JSON(http.StatusOK, gin.H{"message": "acknowledged", "status": models.IncidentStatusInvestigating})
}

// HandleResolve marks an incident resolved and generates the AI postmortem draft.
// POST /incidents/:id/resolve — engineer+.
func (s *Service) HandleResolve(c *gin.Context) {
	userID, incidentID, ok := s.requireIncidentRole(c, models.RoleEngineer)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	tag, err := s.db.Pool.Exec(ctx, `
		UPDATE incidents SET status = 'resolved', resolved_by = $2, resolved_at = NOW()
		WHERE id = $1 AND status <> 'resolved'`, incidentID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve"})
		return
	}
	if tag.RowsAffected() > 0 {
		_, _ = s.postTimelineEntry(ctx, incidentID, models.IncidentAuthorHuman, &userID,
			"Marked resolved.", models.IncidentEntryResolution, nil)
	}
	postmortem, err := s.GeneratePostmortem(ctx, incidentID)
	if err != nil {
		// Resolution succeeded even if postmortem generation hit an error.
		c.JSON(http.StatusOK, gin.H{"status": models.IncidentStatusResolved, "postmortem": postmortem, "postmortem_error": err.Error()})
		return
	}
	s.broadcast(incidentID, "incident_update", gin.H{"status": models.IncidentStatusResolved})
	c.JSON(http.StatusOK, gin.H{"status": models.IncidentStatusResolved, "postmortem": postmortem})
}

// HandleSavePostmortem persists the (possibly edited) postmortem. POST /incidents/:id/postmortem — engineer+.
func (s *Service) HandleSavePostmortem(c *gin.Context) {
	userID, incidentID, ok := s.requireIncidentRole(c, models.RoleEngineer)
	if !ok {
		return
	}
	var req struct {
		Postmortem string `json:"postmortem" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "postmortem is required"})
		return
	}
	ctx := c.Request.Context()
	if _, err := s.db.Pool.Exec(ctx, `UPDATE incidents SET postmortem = $1 WHERE id = $2`, req.Postmortem, incidentID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save postmortem"})
		return
	}
	_, _ = s.postTimelineEntry(ctx, incidentID, models.IncidentAuthorHuman, &userID,
		"Postmortem published.", models.IncidentEntryResolution, nil)
	c.JSON(http.StatusOK, gin.H{"message": "postmortem published"})
}

// HandleApproveAction approves a pending AI/human-proposed action.
// POST /incidents/:id/actions/:actionId/approve — engineer+.
func (s *Service) HandleApproveAction(c *gin.Context) {
	s.decideAction(c, true)
}

// HandleRejectAction rejects a pending action. POST /incidents/:id/actions/:actionId/reject — engineer+.
func (s *Service) HandleRejectAction(c *gin.Context) {
	s.decideAction(c, false)
}

func (s *Service) decideAction(c *gin.Context, approve bool) {
	userID, incidentID, ok := s.requireIncidentRole(c, models.RoleEngineer)
	if !ok {
		return
	}
	actionID, err := uuid.Parse(c.Param("actionId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid action id"})
		return
	}
	ctx := c.Request.Context()

	newStatus := models.IncidentActionRejected
	if approve {
		newStatus = models.IncidentActionApproved
	}
	tag, err := s.db.Pool.Exec(ctx, `
		UPDATE incident_actions
		SET status = $1, approved_by = $2, executed_at = CASE WHEN $4 THEN NOW() ELSE executed_at END
		WHERE id = $3 AND incident_id = $5 AND status = 'pending'`,
		newStatus, userID, actionID, approve, incidentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update action"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "action not found or no longer pending"})
		return
	}

	verb := "Rejected"
	entryType := models.IncidentEntryUpdate
	if approve {
		verb = "Approved"
		entryType = models.IncidentEntryActionTaken
	}
	_, _ = s.postTimelineEntry(ctx, incidentID, models.IncidentAuthorHuman, &userID,
		verb+" a proposed action.", entryType, map[string]any{"action_id": actionID.String()})
	s.broadcast(incidentID, "incident_action", gin.H{"action_id": actionID.String(), "status": newStatus})
	c.JSON(http.StatusOK, gin.H{"message": "action " + newStatus, "status": newStatus})
}

// requireIncidentRole resolves the incident in the path, verifies the caller is a member
// of its org with at least minRole, and returns the user + incident IDs. On failure it
// writes the response and returns ok=false.
func (s *Service) requireIncidentRole(c *gin.Context, minRole string) (userID, incidentID uuid.UUID, ok bool) {
	userID, authed := middleware.GetUserID(c)
	if !authed {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	incidentID, err := uuid.Parse(c.Param("incidentId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid incident id"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	ctx := c.Request.Context()
	orgID, err := s.loadIncidentOrg(ctx, incidentID)
	if errors.Is(err, models.ErrNoMembership) {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load incident"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	role, err := s.db.UserOrgRole(ctx, userID, orgID)
	if errors.Is(err, models.ErrNoMembership) {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	if models.RoleRank(role) < models.RoleRank(minRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "this action requires a higher role in the workspace"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	return userID, incidentID, true
}
