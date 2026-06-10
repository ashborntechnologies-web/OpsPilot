package middleware

import (
	"context"
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

// RequireProjectOwnership is a tenant-isolation guard for routes under /projects/:id.
// It rejects requests where the authenticated user does not own the project in the
// path, returning 404 (not 403) so project existence is not leaked across tenants.
// Must run after RequireAuth. Apply only to a group whose ":id" param is a project ID.
func RequireProjectOwnership(db *models.DB) gin.HandlerFunc {
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
		owned, err := db.UserOwnsProject(c.Request.Context(), userID, projectID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify project access"})
			return
		}
		if !owned {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
		c.Next()
	}
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

	return id, nil
}
