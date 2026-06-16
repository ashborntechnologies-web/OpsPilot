// Package orgs implements team workspaces (organizations) and role-based access
// control. Organizations own all tenant data (projects, AWS accounts, alerts,
// incidents); users access that data via membership with a role (admin/engineer/
// viewer). Tenant isolation + role enforcement live in pkg/middleware; this package
// is CRUD for orgs, members, and invites.
package orgs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/notify"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Service struct {
	db          *models.DB
	emailSvc    *notify.EmailService
	frontendURL string
}

func NewService(db *models.DB, emailSvc *notify.EmailService, frontendURL string) *Service {
	return &Service{db: db, emailSvc: emailSvc, frontendURL: strings.TrimRight(frontendURL, "/")}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify produces a URL-safe slug from a name (lowercase, hyphen-separated).
func slugify(name string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// HandleCreateOrg creates a new organization and makes the caller its admin.
// POST /orgs  body: {name, slug?}
func (s *Service) HandleCreateOrg(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}
	var req struct {
		Name string `json:"name" binding:"required"`
		Slug string `json:"slug"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required (max 100 characters)"})
		return
	}
	base := slugify(req.Slug)
	if base == "" {
		base = slugify(req.Name)
	}
	if base == "" {
		base = "workspace"
	}

	ctx := c.Request.Context()
	slug, err := s.uniqueSlug(ctx, base)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to allocate workspace slug"})
		return
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}
	defer tx.Rollback(ctx)

	var org models.Organization
	if err := tx.QueryRow(ctx,
		`INSERT INTO organizations (name, slug, created_by)
		 VALUES ($1, $2, $3) RETURNING id, name, slug, created_by, created_at, updated_at`,
		req.Name, slug, userID,
	).Scan(&org.ID, &org.Name, &org.Slug, &org.CreatedBy, &org.CreatedAt, &org.UpdatedAt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO organization_members (org_id, user_id, role, invited_by)
		 VALUES ($1, $2, 'admin', $2)`, org.ID, userID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add membership"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}

	org.Role = models.RoleAdmin
	c.JSON(http.StatusCreated, org)
}

// uniqueSlug returns base, or base-2, base-3, … until one is free.
func (s *Service) uniqueSlug(ctx context.Context, base string) (string, error) {
	slug := base
	for i := 2; i < 1000; i++ {
		var exists bool
		if err := s.db.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE slug = $1)`, slug,
		).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return "", errors.New("could not allocate unique slug")
}

// HandleListMyOrgs lists the organizations the caller belongs to, with their role.
// GET /orgs/me
func (s *Service) HandleListMyOrgs(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}
	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT o.id, o.name, o.slug, o.created_by, o.created_at, o.updated_at, m.role,
		        to_char(o.summary_time, 'HH24:MI'), o.summary_timezone, o.summary_enabled
		   FROM organizations o
		   JOIN organization_members m ON m.org_id = o.id
		  WHERE m.user_id = $1
		  ORDER BY o.created_at ASC`, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workspaces"})
		return
	}
	defer rows.Close()

	orgs := []models.Organization{}
	for rows.Next() {
		var o models.Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.Role,
			&o.SummaryTime, &o.SummaryTimezone, &o.SummaryEnabled); err != nil {
			continue
		}
		orgs = append(orgs, o)
	}
	c.JSON(http.StatusOK, orgs)
}

// HandleListMembers lists an org's members with roles + emails. Any member (viewer+).
// GET /orgs/:orgId/members
func (s *Service) HandleListMembers(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT m.id, m.org_id, m.user_id, m.role, m.invited_by, m.joined_at, m.created_at, u.email
		   FROM organization_members m
		   JOIN users u ON u.id = m.user_id
		  WHERE m.org_id = $1
		  ORDER BY m.joined_at ASC`, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list members"})
		return
	}
	defer rows.Close()

	members := []models.OrganizationMember{}
	for rows.Next() {
		var m models.OrganizationMember
		if err := rows.Scan(&m.ID, &m.OrgID, &m.UserID, &m.Role, &m.InvitedBy, &m.JoinedAt, &m.CreatedAt, &m.Email); err != nil {
			continue
		}
		members = append(members, m)
	}
	c.JSON(http.StatusOK, members)
}

// HandleCreateInvite stores an invite and emails the link. Admin only.
// POST /orgs/:orgId/invites  body: {email, role}
func (s *Service) HandleCreateInvite(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	orgID, _ := middleware.GetOrgID(c)

	var req struct {
		Email string `json:"email" binding:"required"`
		Role  string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(req.Email, "@") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid email is required"})
		return
	}
	if !models.ValidRole(req.Role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be one of admin, engineer, viewer"})
		return
	}

	ctx := c.Request.Context()

	// If the invitee is already a member, short-circuit with a clear message.
	var alreadyMember bool
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM organization_members m JOIN users u ON u.id = m.user_id
		    WHERE m.org_id = $1 AND lower(u.email) = $2)`, orgID, req.Email,
	).Scan(&alreadyMember); err == nil && alreadyMember {
		c.JSON(http.StatusConflict, gin.H{"error": "that person is already a member of this workspace"})
		return
	}

	var invite models.OrganizationInvite
	if err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO organization_invites (org_id, email, role, invited_by)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, org_id, email, role, token, invited_by, expires_at, accepted_at, created_at`,
		orgID, req.Email, req.Role, userID,
	).Scan(&invite.ID, &invite.OrgID, &invite.Email, &invite.Role, &invite.Token,
		&invite.InvitedBy, &invite.ExpiresAt, &invite.AcceptedAt, &invite.CreatedAt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create invite"})
		return
	}

	// Send the invite email (best-effort — a no-op when SMTP is unconfigured).
	var orgName string
	_ = s.db.Pool.QueryRow(ctx, `SELECT name FROM organizations WHERE id = $1`, orgID).Scan(&orgName)
	link := fmt.Sprintf("%s/invites/%s", s.frontendURL, invite.Token)
	if err := s.emailSvc.SendOrgInvite(ctx, invite.Email, orgName, req.Role, link); err != nil {
		// Don't fail the request — the admin can re-share the link manually.
		c.JSON(http.StatusCreated, gin.H{"invite": invite, "accept_url": link, "email_sent": false})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"invite": invite, "accept_url": link, "email_sent": true})
}

// HandleAcceptInvite redeems an invite token, adding the caller to the org.
// GET /invites/:token  (requires auth)
func (s *Service) HandleAcceptInvite(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}
	token, err := uuid.Parse(c.Param("token"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invite token"})
		return
	}

	ctx := c.Request.Context()
	var inv models.OrganizationInvite
	err = s.db.Pool.QueryRow(ctx,
		`SELECT id, org_id, email, role, token, invited_by, expires_at, accepted_at, created_at
		   FROM organization_invites WHERE token = $1`, token,
	).Scan(&inv.ID, &inv.OrgID, &inv.Email, &inv.Role, &inv.Token,
		&inv.InvitedBy, &inv.ExpiresAt, &inv.AcceptedAt, &inv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "invite not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load invite"})
		return
	}
	if inv.AcceptedAt != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "this invite has already been accepted"})
		return
	}
	if time.Now().After(inv.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "this invite has expired — ask an admin to send a new one"})
		return
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to accept invite"})
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO organization_members (org_id, user_id, role, invited_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		inv.OrgID, userID, inv.Role, inv.InvitedBy,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add membership"})
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE organization_invites SET accepted_at = NOW() WHERE id = $1`, inv.ID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark invite accepted"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to accept invite"})
		return
	}

	var org models.Organization
	_ = s.db.Pool.QueryRow(ctx,
		`SELECT id, name, slug, created_by, created_at, updated_at FROM organizations WHERE id = $1`,
		inv.OrgID,
	).Scan(&org.ID, &org.Name, &org.Slug, &org.CreatedBy, &org.CreatedAt, &org.UpdatedAt)
	org.Role = inv.Role
	c.JSON(http.StatusOK, gin.H{"message": "joined workspace", "organization": org})
}

// HandleUpdateMemberRole changes a member's role. Admin only. Cannot demote the last
// admin (an org must always retain at least one admin).
// PATCH /orgs/:orgId/members/:userId  body: {role}
func (s *Service) HandleUpdateMemberRole(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	targetID, err := uuid.Parse(c.Param("userId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !models.ValidRole(req.Role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be one of admin, engineer, viewer"})
		return
	}

	ctx := c.Request.Context()
	if req.Role != models.RoleAdmin {
		if last, err := s.isLastAdmin(ctx, orgID, targetID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify admins"})
			return
		} else if last {
			c.JSON(http.StatusConflict, gin.H{"error": "cannot demote the last admin — promote another member to admin first"})
			return
		}
	}

	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE organization_members SET role = $1 WHERE org_id = $2 AND user_id = $3`,
		req.Role, orgID, targetID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update role"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "member not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "role updated", "role": req.Role})
}

// HandleRemoveMember removes a member from the org. Admin only. Cannot remove the
// last admin.
// DELETE /orgs/:orgId/members/:userId
func (s *Service) HandleRemoveMember(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	targetID, err := uuid.Parse(c.Param("userId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	ctx := c.Request.Context()
	if last, err := s.isLastAdmin(ctx, orgID, targetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify admins"})
		return
	} else if last {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot remove the last admin — promote another member to admin first"})
		return
	}

	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM organization_members WHERE org_id = $1 AND user_id = $2`, orgID, targetID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove member"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "member not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "member removed"})
}

// isLastAdmin reports whether targetID is currently an admin and the only admin.
func (s *Service) isLastAdmin(ctx context.Context, orgID, targetID uuid.UUID) (bool, error) {
	var targetIsAdmin bool
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM organization_members WHERE org_id = $1 AND user_id = $2 AND role = 'admin')`,
		orgID, targetID,
	).Scan(&targetIsAdmin); err != nil {
		return false, err
	}
	if !targetIsAdmin {
		return false, nil
	}
	var adminCount int
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM organization_members WHERE org_id = $1 AND role = 'admin'`, orgID,
	).Scan(&adminCount); err != nil {
		return false, err
	}
	return adminCount <= 1, nil
}
