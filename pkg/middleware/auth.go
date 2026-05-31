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
	claims, err := authSvc.ValidateToken(tokenString)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("invalid token: %w", err)
	}
	return upsertUser(ctx, db, authSvc, claims.Subject)
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

		userID, err := ResolveToken(c.Request.Context(), db, authSvc, parts[1])
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		claims, _ := authSvc.ValidateToken(parts[1])
		c.Set(ClerkIDContextKey, claims.Subject)
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
