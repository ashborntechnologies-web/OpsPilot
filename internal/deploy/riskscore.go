// riskscore.go computes a pre-deploy risk score from the project's recent
// operational history (failures, open alerts, anomalies, degraded capacity,
// timing). The score is advisory — it is broadcast to the UI before a deploy
// starts but never blocks the API.
package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

// Risk level constants
const (
	RiskLow      = "low"
	RiskMedium   = "medium"
	RiskHigh     = "high"
	RiskCritical = "critical"
)

// RiskFactor is one scored signal contributing to the total.
type RiskFactor struct {
	Name   string `json:"name"`
	Points int    `json:"points"`
	Reason string `json:"reason"`
}

// RiskScore is the advisory pre-deploy risk assessment.
type RiskScore struct {
	Score       int          `json:"score"` // 0-100, higher = riskier
	Level       string       `json:"level"` // low | medium | high | critical
	Factors     []RiskFactor `json:"factors"`
	Explanation string       `json:"explanation"`
}

// riskSignals are the raw inputs to scoring, gathered from the DB. Separated
// from scoring so the scoring logic is a pure, testable function.
type riskSignals struct {
	LastDeployFailed   bool
	OpenAlerts         int
	LogAnomaliesLastHr int
	TasksDegraded      bool
	IsFridayAfternoon  bool
	CommitFailedBefore bool
	RollbacksLast7d    int
	LastThreeSucceeded bool
}

// ScoreFromSignals converts gathered signals into a RiskScore. Pure function.
func ScoreFromSignals(s riskSignals) *RiskScore {
	rs := &RiskScore{Factors: []RiskFactor{}}
	add := func(name string, points int, reason string) {
		rs.Factors = append(rs.Factors, RiskFactor{Name: name, Points: points, Reason: reason})
		rs.Score += points
	}

	if s.LastDeployFailed {
		add("last_deploy_failed", 30, "The most recent deployment to this environment failed.")
	}
	if s.OpenAlerts > 0 {
		add("open_alerts", 20, fmt.Sprintf("%d alert(s) are currently open for this environment.", s.OpenAlerts))
	}
	if s.LogAnomaliesLastHr > 0 {
		add("recent_log_anomalies", 15, fmt.Sprintf("%d log anomaly event(s) in the last hour.", s.LogAnomaliesLastHr))
	}
	if s.TasksDegraded {
		add("tasks_degraded", 15, "The service is currently running fewer tasks than desired.")
	}
	if s.IsFridayAfternoon {
		add("friday_afternoon", 10, "Friday afternoon deploys are statistically riskier.")
	}
	if s.CommitFailedBefore {
		add("commit_failed_before", 10, "This commit previously failed to deploy in another environment.")
	}
	if s.RollbacksLast7d >= 2 {
		add("frequent_rollbacks", 5, fmt.Sprintf("%d rollbacks in the last 7 days.", s.RollbacksLast7d))
	}
	if s.LastThreeSucceeded {
		add("recent_success_streak", -10, "The last 3 deployments all succeeded.")
	}

	if rs.Score < 0 {
		rs.Score = 0
	}
	if rs.Score > 100 {
		rs.Score = 100
	}

	switch {
	case rs.Score <= 20:
		rs.Level = RiskLow
	case rs.Score <= 40:
		rs.Level = RiskMedium
	case rs.Score <= 65:
		rs.Level = RiskHigh
	default:
		rs.Level = RiskCritical
	}
	return rs
}

// isFridayAfternoonUTC reports whether t is Friday 14:00-18:00 UTC.
func isFridayAfternoonUTC(t time.Time) bool {
	utc := t.UTC()
	return utc.Weekday() == time.Friday && utc.Hour() >= 14 && utc.Hour() < 18
}

// ComputeRiskScore gathers risk signals for a pending deploy and scores them.
// When the score is 40+, an LLM-written one-sentence explanation is attached.
func (s *Service) ComputeRiskScore(ctx context.Context, projectID uuid.UUID, envName, commitSHA string) (*RiskScore, error) {
	env, err := s.getEnvironment(ctx, projectID, envName)
	if err != nil {
		return nil, err
	}

	var sig riskSignals
	sig.IsFridayAfternoon = isFridayAfternoonUTC(time.Now())

	// One round-trip for all DB-derived signals.
	err = s.db.Pool.QueryRow(ctx, `
		SELECT
			COALESCE((SELECT status = 'failed' FROM deployments
			          WHERE environment_id = $1 AND status IN ('live','failed','rolled_back')
			          ORDER BY created_at DESC LIMIT 1), false),
			(SELECT COUNT(*) FROM alerts
			  WHERE project_id = $2 AND environment_id = $1 AND status = 'open'),
			(SELECT COUNT(*) FROM operational_events
			  WHERE environment_id = $1 AND event_type = $3
			    AND occurred_at > NOW() - INTERVAL '1 hour'),
			COALESCE((SELECT true FROM deployments
			          WHERE project_id = $2 AND commit_sha = $4 AND status = 'failed' LIMIT 1), false),
			(SELECT COUNT(*) FROM operational_events
			  WHERE project_id = $2 AND event_type = $5
			    AND occurred_at > NOW() - INTERVAL '7 days'),
			COALESCE((SELECT COUNT(*) = 3 FROM (
				SELECT status FROM deployments
				WHERE environment_id = $1 AND status IN ('live','failed','rolled_back')
				ORDER BY created_at DESC LIMIT 3
			) r WHERE r.status IN ('live','rolled_back')), false)`,
		env.ID, projectID, models.EventRuntimeLogAnomaly, commitSHA, models.EventRollbackTriggered,
	).Scan(&sig.LastDeployFailed, &sig.OpenAlerts, &sig.LogAnomaliesLastHr,
		&sig.CommitFailedBefore, &sig.RollbacksLast7d, &sig.LastThreeSucceeded)
	if err != nil {
		return nil, err
	}

	// Live capacity check via the last health snapshot is owned by the poller;
	// here we approximate from recent degraded events (no extra AWS call on the
	// deploy hot path).
	var degradedRecently bool
	s.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM operational_events
			WHERE environment_id = $1
			  AND event_type IN ($2, $3)
			  AND occurred_at > NOW() - INTERVAL '10 minutes'
		)`, env.ID, models.EventRuntimeTasksDegraded, models.EventRuntimeServiceDown,
	).Scan(&degradedRecently)
	sig.TasksDegraded = degradedRecently

	rs := ScoreFromSignals(sig)

	if rs.Score >= 40 {
		rs.Explanation = s.explainRisk(ctx, rs)
	}
	return rs, nil
}

// explainRisk asks the LLM for a one-sentence plain-English explanation of the
// top factors. Falls back to the highest-point factor's reason.
func (s *Service) explainRisk(ctx context.Context, rs *RiskScore) string {
	var parts []string
	for _, f := range rs.Factors {
		if f.Points > 0 {
			parts = append(parts, fmt.Sprintf("%s (+%d): %s", f.Name, f.Points, f.Reason))
		}
	}
	fallback := ""
	if len(parts) > 0 {
		fallback = rs.Factors[0].Reason
	}
	if s.riskLLM == nil {
		return fallback
	}

	llmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := s.riskLLM.Complete(llmCtx, "",
		fmt.Sprintf("In one sentence (max 150 characters), explain to a developer why this deploy is risky right now. Factors:\n%s\nRespond with the sentence only.",
			strings.Join(parts, "\n")), 100)
	if err != nil || strings.TrimSpace(out) == "" {
		return fallback
	}
	out = strings.TrimSpace(out)
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}
