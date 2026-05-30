package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/convdeploy/platform/internal/auth"
	"github.com/convdeploy/platform/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const UserIDContextKey = "user_id"   // uuid.UUID
const ClerkIDContextKey = "clerk_id" // string

// RequireAuth validates the Clerk JWT, upserts the user in Postgres, and sets
// user_id (uuid.UUID) and clerk_id (string) on the Gin context for downstream handlers.
func RequireAuth(authSvc *auth.Service, db *models.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Prefer Authorization header; fall back to ?token= query param.
		// The query-param fallback is required for WebSocket upgrades — browsers
		// cannot set custom headers on the native WebSocket API.
		tokenString := ""
		if authHeader := c.GetHeader("Authorization"); authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
				return
			}
			tokenString = parts[1]
		} else if t := c.Query("token"); t != "" {
			tokenString = t
		}

		if tokenString == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
			return
		}

		claims, err := authSvc.ValidateToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		clerkID := claims.Subject

		userID, err := upsertUser(c.Request.Context(), db, authSvc, clerkID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve user"})
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
		return uuid.UUID{}, nil // should not happen for real users
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
