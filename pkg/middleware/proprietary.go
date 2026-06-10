package middleware

import (
	"os"

	"github.com/gin-gonic/gin"
)

// Version returns the running platform version (VERSION env var, default 1.0.0).
func Version() string {
	if v := os.Getenv("VERSION"); v != "" {
		return v
	}
	return "1.0.0"
}

// Proprietary stamps every response with OpsPilot identification headers and
// strips framework defaults so the platform controls its own identity surface.
func Proprietary() gin.HandlerFunc {
	version := Version()
	return func(c *gin.Context) {
		c.Header("X-Powered-By", "OpsPilot")
		c.Header("X-OpsPilot-Version", version)
		c.Header("X-Terms", "https://opspilot.dev/terms")
		c.Next()
	}
}
