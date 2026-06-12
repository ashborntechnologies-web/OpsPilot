package discovery

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ScanEnqueuer enqueues an async account scan and returns the job ID. Implemented by
// the queue client; injected via SetEnqueuer to avoid a discovery↔queue import cycle.
type ScanEnqueuer interface {
	EnqueueScan(accountID string) (jobID string, err error)
}

// SetEnqueuer injects the scan-job enqueuer used by HandleScanAccount.
func (s *Service) SetEnqueuer(e ScanEnqueuer) { s.enqueuer = e }

// HandleScanAccount triggers an async full scan of an AWS account.
// POST /aws-accounts/:id/scan — member of the account's org, engineer+.
func (s *Service) HandleScanAccount(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}
	accountID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}
	if _, role, ok := s.requireAccountRole(c, userID, accountID, models.RoleEngineer); !ok {
		_ = role
		return
	}
	if s.enqueuer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "scanning is not available"})
		return
	}
	jobID, err := s.enqueuer.EnqueueScan(accountID.String())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue scan"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": jobID, "message": "scan started"})
}

// HandleListOrgResources lists discovered resources for an org with optional filters.
// GET /orgs/:orgId/resources?resource_type=&region=&project_id=(uuid|null) — any member.
func (s *Service) HandleListOrgResources(c *gin.Context) {
	orgID, ok := middleware.GetOrgID(c) // set by RequireOrgMembership
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "organization not resolved"})
		return
	}

	where := []string{"org_id = $1"}
	args := []any{orgID}
	add := func(clause string, val any) {
		args = append(args, val)
		where = append(where, clause+" $"+strconv.Itoa(len(args)))
	}
	if rt := c.Query("resource_type"); rt != "" {
		add("resource_type =", rt)
	}
	if region := c.Query("region"); region != "" {
		add("region =", region)
	}
	switch pid := c.Query("project_id"); pid {
	case "":
		// no project filter
	case "null", "unassigned":
		where = append(where, "project_id IS NULL")
	default:
		parsed, err := uuid.Parse(pid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id filter"})
			return
		}
		add("project_id =", parsed)
	}

	query := `SELECT id, org_id, aws_account_id, resource_type, resource_id, resource_name,
	                 region, metadata, tags, project_id, is_managed, first_seen_at, last_seen_at
	          FROM discovered_resources WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY resource_type, resource_name`

	s.writeResourceList(c, query, args...)
}

// HandleListProjectResources lists resources assigned to a project (managed +
// discovered). GET /projects/:id/resources — any member (LoadProjectMembership ran).
func (s *Service) HandleListProjectResources(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	query := `SELECT id, org_id, aws_account_id, resource_type, resource_id, resource_name,
	                 region, metadata, tags, project_id, is_managed, first_seen_at, last_seen_at
	          FROM discovered_resources WHERE project_id = $1
	          ORDER BY resource_type, resource_name`
	s.writeResourceList(c, query, projectID)
}

// HandleAssignResource assigns (or, with null, unassigns) a discovered resource to a
// project. PATCH /resources/:resourceId/assign — engineer+ in the resource's org.
func (s *Service) HandleAssignResource(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}
	resourceID, err := uuid.Parse(c.Param("resourceId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid resource id"})
		return
	}
	var req struct {
		ProjectID *string `json:"project_id"` // null/omitted unassigns
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	var resourceOrg uuid.UUID
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT org_id FROM discovered_resources WHERE id = $1`, resourceID,
	).Scan(&resourceOrg); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load resource"})
		return
	}

	role, err := s.db.UserOrgRole(ctx, userID, resourceOrg)
	if errors.Is(err, models.ErrNoMembership) {
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		return
	}
	if models.RoleRank(role) < models.RoleRank(models.RoleEngineer) {
		c.JSON(http.StatusForbidden, gin.H{"error": "assigning resources requires the engineer or admin role"})
		return
	}

	var projectID *uuid.UUID
	if req.ProjectID != nil && *req.ProjectID != "" {
		parsed, err := uuid.Parse(*req.ProjectID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
			return
		}
		// The target project must belong to the same org as the resource.
		var projOrg uuid.UUID
		if err := s.db.Pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, parsed).Scan(&projOrg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "project not found"})
			return
		}
		if projOrg != resourceOrg {
			c.JSON(http.StatusBadRequest, gin.H{"error": "project belongs to a different workspace than the resource"})
			return
		}
		projectID = &parsed
	}

	if _, err := s.db.Pool.Exec(ctx,
		`UPDATE discovered_resources SET project_id = $1 WHERE id = $2`, projectID, resourceID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to assign resource"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "resource assigned", "project_id": projectID})
}

// requireAccountRole verifies the user is a member of the account's org with at least
// minRole, returning the org ID and role. On failure it writes the response and ok=false.
func (s *Service) requireAccountRole(c *gin.Context, userID, accountID uuid.UUID, minRole string) (uuid.UUID, string, bool) {
	ctx := c.Request.Context()
	var orgID uuid.UUID
	if err := s.db.Pool.QueryRow(ctx, `SELECT org_id FROM aws_accounts WHERE id = $1`, accountID).Scan(&orgID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "AWS account not found"})
		return uuid.UUID{}, "", false
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load account"})
		return uuid.UUID{}, "", false
	}
	role, err := s.db.UserOrgRole(ctx, userID, orgID)
	if errors.Is(err, models.ErrNoMembership) {
		c.JSON(http.StatusNotFound, gin.H{"error": "AWS account not found"})
		return uuid.UUID{}, "", false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		return uuid.UUID{}, "", false
	}
	if models.RoleRank(role) < models.RoleRank(minRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "this action requires a higher role in the workspace"})
		return uuid.UUID{}, "", false
	}
	return orgID, role, true
}

// writeResourceList runs a resource query and writes the JSON array, decoding the
// metadata/tags JSONB columns into maps.
func (s *Service) writeResourceList(c *gin.Context, query string, args ...any) {
	rows, err := s.db.Pool.Query(c.Request.Context(), query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list resources"})
		return
	}
	defer rows.Close()

	resources := []models.DiscoveredResource{}
	for rows.Next() {
		var r models.DiscoveredResource
		var metaRaw, tagsRaw []byte
		if err := rows.Scan(&r.ID, &r.OrgID, &r.AWSAccountID, &r.ResourceType, &r.ResourceID,
			&r.ResourceName, &r.Region, &metaRaw, &tagsRaw, &r.ProjectID, &r.IsManaged,
			&r.FirstSeenAt, &r.LastSeenAt); err != nil {
			continue
		}
		_ = json.Unmarshal(metaRaw, &r.Metadata)
		_ = json.Unmarshal(tagsRaw, &r.Tags)
		resources = append(resources, r)
	}
	c.JSON(http.StatusOK, resources)
}
