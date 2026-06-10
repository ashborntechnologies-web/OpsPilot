package deploy

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HealthScore is a 0-100 composite of deployment reliability signals, computed
// entirely from platform data (no AWS calls) so it is fast and always available.
type HealthScore struct {
	Score      int            `json:"score"`
	Grade      string         `json:"grade"` // healthy | degraded | at_risk | critical
	Components map[string]int `json:"components"`
	Insights   []string       `json:"insights"`
}

// HandleGetHealthScore returns the project's deployment health score.
func (s *Service) HandleGetHealthScore(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	score, err := s.ComputeHealthScore(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to compute health score"})
		return
	}
	c.JSON(http.StatusOK, score)
}

// ComputeHealthScore aggregates reliability signals:
//   - deploy success rate over the last 10 deployments (0-50 points)
//   - environment readiness (0-25 points)
//   - error-severity operational events in the last 24h (0-15 points)
//   - rollbacks in the last 7 days (0-10 points)
func (s *Service) ComputeHealthScore(ctx context.Context, projectID uuid.UUID) (*HealthScore, error) {
	hs := &HealthScore{Components: map[string]int{}, Insights: []string{}}

	// Success rate of last 10 finished deployments.
	var total, succeeded int
	err := s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE status IN ('live', 'rolled_back'))
		FROM (
			SELECT status FROM deployments
			WHERE project_id = $1 AND status IN ('live', 'failed', 'rolled_back')
			ORDER BY created_at DESC LIMIT 10
		) recent`, projectID,
	).Scan(&total, &succeeded)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		hs.Components["deploy_success"] = 50 // no history — assume healthy
		hs.Insights = append(hs.Insights, "No deployments yet — deploy to start tracking reliability.")
	} else {
		hs.Components["deploy_success"] = 50 * succeeded / total
		if succeeded < total {
			hs.Insights = append(hs.Insights, "Some recent deployments failed — run a diagnosis on the latest failure to find the root cause.")
		}
	}

	// Environment readiness.
	var envTotal, envReady int
	err = s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE stack_status = 'ready')
		FROM environments WHERE project_id = $1 AND is_preview = false`, projectID,
	).Scan(&envTotal, &envReady)
	if err != nil {
		return nil, err
	}
	if envTotal == 0 {
		hs.Components["environments"] = 0
		hs.Insights = append(hs.Insights, "No environments created — create a production environment to deploy.")
	} else {
		hs.Components["environments"] = 25 * envReady / envTotal
		if envReady < envTotal {
			hs.Insights = append(hs.Insights, "One or more environments are not ready — check provisioning status.")
		}
	}

	// Error events in the last 24h: full points at 0 errors, -3 per error.
	var errors24h int
	err = s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM operational_events
		WHERE project_id = $1 AND severity = 'error' AND occurred_at > NOW() - INTERVAL '24 hours'`,
		projectID,
	).Scan(&errors24h)
	if err != nil {
		return nil, err
	}
	stability := 15 - 3*errors24h
	if stability < 0 {
		stability = 0
	}
	hs.Components["stability_24h"] = stability
	if errors24h > 0 {
		hs.Insights = append(hs.Insights, "Errors occurred in the last 24 hours — review the deployment event timeline.")
	}

	// Rollbacks in the last 7 days: full points at 0, -5 per rollback.
	var rollbacks7d int
	err = s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM operational_events
		WHERE project_id = $1 AND event_type = 'rollback.triggered' AND occurred_at > NOW() - INTERVAL '7 days'`,
		projectID,
	).Scan(&rollbacks7d)
	if err != nil {
		return nil, err
	}
	rollbackScore := 10 - 5*rollbacks7d
	if rollbackScore < 0 {
		rollbackScore = 0
	}
	hs.Components["rollbacks_7d"] = rollbackScore
	if rollbacks7d > 0 {
		hs.Insights = append(hs.Insights, "Recent rollbacks detected — recent releases may be unstable.")
	}

	for _, v := range hs.Components {
		hs.Score += v
	}
	switch {
	case hs.Score >= 85:
		hs.Grade = "healthy"
	case hs.Score >= 65:
		hs.Grade = "degraded"
	case hs.Score >= 40:
		hs.Grade = "at_risk"
	default:
		hs.Grade = "critical"
	}
	return hs, nil
}
