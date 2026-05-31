package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// RateLimiter tracks a per-user token-bucket limiter.
// Create with NewRateLimiter and attach via its Middleware() method.
type RateLimiter struct {
	mu    sync.Mutex
	users map[uuid.UUID]*entry
	rps   rate.Limit
	burst int
}

// NewRateLimiter creates a limiter allowing rps requests per second with the
// given burst size. Stale entries are purged every 5 minutes.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		users: make(map[uuid.UUID]*entry),
		rps:   rate.Limit(rps),
		burst: burst,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) get(userID uuid.UUID) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e, ok := rl.users[userID]
	if !ok {
		e = &entry{limiter: rate.NewLimiter(rl.rps, rl.burst)}
		rl.users[userID] = e
	}
	e.lastSeen = time.Now()
	return e.limiter
}

func (rl *RateLimiter) cleanup() {
	for range time.Tick(5 * time.Minute) {
		cutoff := time.Now().Add(-10 * time.Minute)
		rl.mu.Lock()
		for id, e := range rl.users {
			if e.lastSeen.Before(cutoff) {
				delete(rl.users, id)
			}
		}
		rl.mu.Unlock()
	}
}

// Middleware returns a Gin handler that enforces the rate limit per authenticated user.
// Requests from unauthenticated callers are passed through (auth middleware handles rejection).
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := GetUserID(c)
		if !ok {
			c.Next()
			return
		}
		if !rl.get(userID).Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded — please slow down",
			})
			return
		}
		c.Next()
	}
}
