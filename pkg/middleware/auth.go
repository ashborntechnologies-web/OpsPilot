package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ashborntechnologies-web/OpsPilot/internal/auth"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const UserIDContextKey = "user_id"   // uuid.UUID
const ClerkIDContextKey = "clerk_id" // string
const OrgIDContextKey = "org_id"     // uuid.UUID — set by org/project membership loaders
const OrgRoleContextKey = "org_role" // string — the caller's role in OrgIDContextKey

// ActiveOrgHeader names the request header the frontend uses to select which org a
// request targets for non-project routes (e.g. listing/creating projects or AWS
// accounts). Absent → the caller's personal (oldest) org.
const ActiveOrgHeader = "X-Org-Id"

// ResolveToken validates a raw Clerk JWT string and returns the platform user ID.
// Used by both the HTTP middleware and the WebSocket first-message auth handler.
func ResolveToken(ctx context.Context, db *models.DB, authSvc *auth.Service, tokenString string) (uuid.UUID, error) {
	userID, _, err := resolveTokenWithClerkID(ctx, db, authSvc, tokenString)
	return userID, err
}

func resolveTokenWithClerkID(ctx context.Context, db *models.DB, authSvc *auth.Service, tokenString string) (uuid.UUID, string, error) {
	claims, err := authSvc.ValidateToken(tokenString)
	if err != nil {
		return uuid.UUID{}, "", fmt.Errorf("invalid token: %w", err)
	}
	userID, err := upsertUser(ctx, db, authSvc, claims.Subject)
	return userID, claims.Subject, err
}

// RequireAuth validates the Clerk JWT from the Authorization header, upserts the user
// in Postgres, and sets user_id / clerk_id on the Gin context for downstream handlers.
func RequireAuth(authSvc *auth.Service, db *models.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			return
		}

		userID, clerkID, err := resolveTokenWithClerkID(c.Request.Context(), db, authSvc, parts[1])
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		c.Set(ClerkIDContextKey, clerkID)
		c.Set(UserIDContextKey, userID)
		c.Next()
	}
}

// GetUserID extracts the authenticated user's UUID from the Gin context.
func GetUserID(c *gin.Context) (uuid.UUID, bool) {
	v, exists := c.Get(UserIDContextKey)
	if !exists {
		return uuid.UUID{}, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

// GetOrgID / GetOrgRole read the active org and the caller's role within it, as set
// by LoadProjectMembership or RequireOrgMembership.
func GetOrgID(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(OrgIDContextKey)
	if !ok {
		return uuid.UUID{}, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

func GetOrgRole(c *gin.Context) (string, bool) {
	v, ok := c.Get(OrgRoleContextKey)
	if !ok {
		return "", false
	}
	role, ok := v.(string)
	return role, ok
}

// roleSatisfies reports whether userRole meets the requirement. Roles are
// hierarchical (admin > engineer > viewer): the requirement is the *minimum* rank
// among the allowed roles, so requiring "engineer" is also satisfied by "admin".
// An empty allowed list means "any member" (viewer or above).
func roleSatisfies(userRole string, allowed []string) bool {
	min := models.RoleRank(models.RoleViewer)
	if len(allowed) > 0 {
		min = models.RoleRank(allowed[0])
		for _, r := range allowed[1:] {
			if rank := models.RoleRank(r); rank < min {
				min = rank
			}
		}
	}
	return models.RoleRank(userRole) >= min && models.RoleRank(userRole) > 0
}

// LoadProjectMembership is the tenant-isolation guard for routes under /projects/:id.
// It resolves the org that owns the project and the caller's role in that org,
// storing both on the context. Non-members get 404 (not 403) so project existence
// is not leaked across tenants. Must run after RequireAuth. Pair with RequireRole
// on action routes to enforce the role hierarchy.
func LoadProjectMembership(db *models.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := GetUserID(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
			return
		}
		projectID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
			return
		}
		orgID, role, err := db.ProjectOrgRole(c.Request.Context(), userID, projectID)
		if errors.Is(err, models.ErrNoMembership) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify project access"})
			return
		}
		c.Set(OrgIDContextKey, orgID)
		c.Set(OrgRoleContextKey, role)
		c.Next()
	}
}

// RequireRole enforces that the caller's already-loaded org role satisfies the
// requirement (see roleSatisfies). It performs no DB query — a membership loader
// (LoadProjectMembership or RequireOrgMembership) must have run first. Use it to
// gate action routes, e.g. RequireRole(models.RoleEngineer) for deploy/rollback or
// RequireRole(models.RoleAdmin) for destructive/admin actions.
func RequireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		role, ok := GetOrgRole(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "membership not loaded"})
			return
		}
		if !roleSatisfies(role, roles) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": fmt.Sprintf("this action requires a higher role — your role in this workspace is %q", role),
			})
			return
		}
		c.Next()
	}
}

// RequireOrgMembership guards routes under /orgs/:orgId. It validates the caller is
// a member of the org with a sufficient role (hierarchical; empty = any member) and
// stores org_id + role on the context. Non-members get 404 to avoid leaking org
// existence. Must run after RequireAuth.
func RequireOrgMembership(db *models.DB, roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := GetUserID(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
			return
		}
		orgID, err := uuid.Parse(c.Param("orgId"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid organization id"})
			return
		}
		role, err := db.UserOrgRole(c.Request.Context(), userID, orgID)
		if errors.Is(err, models.ErrNoMembership) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify organization access"})
			return
		}
		if !roleSatisfies(role, roles) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": fmt.Sprintf("this action requires a higher role — your role in this workspace is %q", role),
			})
			return
		}
		c.Set(OrgIDContextKey, orgID)
		c.Set(OrgRoleContextKey, role)
		c.Next()
	}
}

// ActiveOrg resolves which org a non-project-scoped request targets: the X-Org-Id
// header if present (membership-validated), otherwise the caller's personal (oldest)
// org. On failure it writes a JSON error and returns ok=false, so handlers can
// `if orgID, role, ok := middleware.ActiveOrg(c, db); !ok { return }`.
func ActiveOrg(c *gin.Context, db *models.DB) (orgID uuid.UUID, role string, ok bool) {
	userID, authed := GetUserID(c)
	if !authed {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return uuid.UUID{}, "", false
	}
	ctx := c.Request.Context()

	if header := strings.TrimSpace(c.GetHeader(ActiveOrgHeader)); header != "" {
		parsed, err := uuid.Parse(header)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + ActiveOrgHeader + " header"})
			return uuid.UUID{}, "", false
		}
		role, err := db.UserOrgRole(ctx, userID, parsed)
		if errors.Is(err, models.ErrNoMembership) {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return uuid.UUID{}, "", false
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify organization access"})
			return uuid.UUID{}, "", false
		}
		return parsed, role, true
	}

	// Default to the caller's personal (oldest) org.
	err := db.Pool.QueryRow(ctx,
		`SELECT m.org_id, m.role
		   FROM organization_members m
		   JOIN organizations o ON o.id = m.org_id
		  WHERE m.user_id = $1
		  ORDER BY o.created_at ASC
		  LIMIT 1`, userID,
	).Scan(&orgID, &role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "no organization found for user"})
		return uuid.UUID{}, "", false
	}
	return orgID, role, true
}

// upsertUser looks up the user by clerk_id; if not found, fetches from Clerk and inserts.
func upsertUser(ctx context.Context, db *models.DB, authSvc *auth.Service, clerkID string) (uuid.UUID, error) {
	// Fast path: user already exists
	var id uuid.UUID
	err := db.Pool.QueryRow(ctx,
		`SELECT id FROM users WHERE clerk_id = $1`, clerkID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}

	// User not found — fetch details from Clerk and insert
	clerkUser, err := authSvc.FetchClerkUser(ctx, clerkID)
	if err != nil {
		return uuid.UUID{}, err
	}

	email := clerkUser.PrimaryEmail()
	if email == "" {
		return uuid.UUID{}, fmt.Errorf("clerk user %s has no primary email", clerkID)
	}

	// INSERT ... ON CONFLICT handles the race between concurrent first-logins
	err = db.Pool.QueryRow(ctx,
		`INSERT INTO users (clerk_id, email)
		 VALUES ($1, $2)
		 ON CONFLICT (clerk_id) DO UPDATE SET updated_at = NOW()
		 RETURNING id`,
		clerkID, email,
	).Scan(&id)
	if err != nil {
		return uuid.UUID{}, err
	}

	// Every user belongs to at least their personal organization. Existing users
	// are migrated by backfillPersonalOrgs; brand-new users get theirs here.
	if err := ensurePersonalOrg(ctx, db, id, email); err != nil {
		return uuid.UUID{}, err
	}

	return id, nil
}

// ensurePersonalOrg creates a personal organization (admin membership) for a user
// who has none. Idempotent and safe under concurrent first-logins.
func ensurePersonalOrg(ctx context.Context, db *models.DB, userID uuid.UUID, email string) error {
	var hasMembership bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM organization_members WHERE user_id = $1)`, userID,
	).Scan(&hasMembership); err != nil {
		return err
	}
	if hasMembership {
		return nil
	}

	name := personalOrgName(email)
	slug := "u-" + strings.ReplaceAll(userID.String(), "-", "")

	var orgID uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug, created_by)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (slug) DO UPDATE SET updated_at = NOW()
		 RETURNING id`,
		name, slug, userID,
	).Scan(&orgID); err != nil {
		return err
	}

	_, err := db.Pool.Exec(ctx,
		`INSERT INTO organization_members (org_id, user_id, role, invited_by)
		 VALUES ($1, $2, 'admin', $2)
		 ON CONFLICT (org_id, user_id) DO NOTHING`,
		orgID, userID,
	)
	return err
}

// personalOrgName derives a friendly workspace name from the email local part.
func personalOrgName(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		local = email[:i]
	}
	local = strings.TrimSpace(local)
	if local == "" {
		return "Personal"
	}
	return strings.Title(local) + " (personal)" //nolint:staticcheck // simple title-case is fine here
}
