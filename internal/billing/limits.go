// Package billing defines plan tiers and enforces usage limits — project count
// caps and monthly AI-action metering. There is no payment integration yet;
// plans are set directly on the users table.
package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

// PlanLimits describes what a plan tier allows.
type PlanLimits struct {
	MaxProjects       int
	MaxEnvironments   int
	AIActionsPerMonth int
}

// limits per plan tier. Pro/Team AI actions are effectively unlimited.
var planLimits = map[string]PlanLimits{
	models.PlanFree: {MaxProjects: 1, MaxEnvironments: 1, AIActionsPerMonth: 10},
	models.PlanPro:  {MaxProjects: 5, MaxEnvironments: 10, AIActionsPerMonth: 999999},
	models.PlanTeam: {MaxProjects: 999, MaxEnvironments: 999, AIActionsPerMonth: 999999},
}

// LimitsFor returns the limits for a plan, defaulting to Free for unknown values.
func LimitsFor(plan string) PlanLimits {
	if l, ok := planLimits[plan]; ok {
		return l
	}
	return planLimits[models.PlanFree]
}

// ErrLimitReached is returned when a plan limit blocks an action. The message
// is safe to surface to the user.
type ErrLimitReached struct{ Message string }

func (e *ErrLimitReached) Error() string { return e.Message }

type Service struct {
	db *models.DB
}

func NewService(db *models.DB) *Service {
	return &Service{db: db}
}

// CheckProjectLimit returns ErrLimitReached when the user is at their plan's
// project cap.
func (s *Service) CheckProjectLimit(ctx context.Context, userID uuid.UUID) error {
	var plan string
	var count int
	err := s.db.Pool.QueryRow(ctx, `
		SELECT u.plan, (SELECT COUNT(*) FROM projects WHERE user_id = u.id)
		FROM users u WHERE u.id = $1`, userID,
	).Scan(&plan, &count)
	if err != nil {
		return err
	}
	limits := LimitsFor(plan)
	if count >= limits.MaxProjects {
		return &ErrLimitReached{Message: fmt.Sprintf(
			"Your %s plan allows %d project(s) — upgrade to create more.", plan, limits.MaxProjects)}
	}
	return nil
}

// IncrementAIAction atomically meters one AI action against the user's monthly
// allowance, resetting the counter when the 30-day window has elapsed. Returns
// ErrLimitReached when the allowance is exhausted.
func (s *Service) IncrementAIAction(ctx context.Context, userID uuid.UUID) error {
	// Reset stale windows first (no-op when within the window).
	_, err := s.db.Pool.Exec(ctx, `
		UPDATE users SET ai_actions_this_month = 0, ai_actions_reset_at = NOW()
		WHERE id = $1 AND ai_actions_reset_at < NOW() - INTERVAL '30 days'`, userID)
	if err != nil {
		return err
	}

	var plan string
	var used int
	err = s.db.Pool.QueryRow(ctx, `
		UPDATE users SET ai_actions_this_month = ai_actions_this_month + 1
		WHERE id = $1
		RETURNING plan, ai_actions_this_month`, userID,
	).Scan(&plan, &used)
	if err != nil {
		return err
	}

	limits := LimitsFor(plan)
	if used > limits.AIActionsPerMonth {
		// Refund the increment so retries after upgrade aren't penalized.
		s.db.Pool.Exec(ctx, `UPDATE users SET ai_actions_this_month = ai_actions_this_month - 1 WHERE id = $1`, userID)
		return &ErrLimitReached{Message: fmt.Sprintf(
			"You've used all %d AI actions on the %s plan this month — upgrade for unlimited AI.",
			limits.AIActionsPerMonth, plan)}
	}
	return nil
}

// Usage is the current consumption snapshot for a user, served by /users/me.
type Usage struct {
	Plan               string    `json:"plan"`
	AIActionsThisMonth int       `json:"ai_actions_this_month"`
	AIActionsLimit     int       `json:"ai_actions_limit"`
	ProjectsCount      int       `json:"projects_count"`
	ProjectsLimit      int       `json:"projects_limit"`
	ResetAt            time.Time `json:"ai_actions_reset_at"`
}

// GetUsage returns the user's plan and consumption.
func (s *Service) GetUsage(ctx context.Context, userID uuid.UUID) (*Usage, error) {
	var u Usage
	err := s.db.Pool.QueryRow(ctx, `
		SELECT u.plan, u.ai_actions_this_month, u.ai_actions_reset_at,
		       (SELECT COUNT(*) FROM projects WHERE user_id = u.id)
		FROM users u WHERE u.id = $1`, userID,
	).Scan(&u.Plan, &u.AIActionsThisMonth, &u.ResetAt, &u.ProjectsCount)
	if err != nil {
		return nil, err
	}
	limits := LimitsFor(u.Plan)
	u.AIActionsLimit = limits.AIActionsPerMonth
	u.ProjectsLimit = limits.MaxProjects
	return &u, nil
}
