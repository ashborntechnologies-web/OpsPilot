package middleware

import (
	"github.com/gin-gonic/gin"
)

// CORS restricts cross-origin requests to the configured frontend. With no
// FRONTEND_URL set (local dev) the request origin is reflected, allowing all.
// It also sets baseline security headers on every response.
func CORS(frontendURL string) gin.HandlerFunc {
	return func(c *gin.Context) {
		allowOrigin := frontendURL
		if allowOrigin == "" {
			allowOrigin = c.GetHeader("Origin")
		}
		if allowOrigin != "" {
			c.Header("Access-Control-Allow-Origin", allowOrigin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization")
			c.Header("Access-Control-Allow-Credentials", "true")
		}

		// Baseline security headers — the API serves JSON only.
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}
