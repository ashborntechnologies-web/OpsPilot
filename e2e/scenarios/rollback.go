package scenarios

import (
	"context"
	"fmt"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/e2e/catalog"
	"github.com/ashborntechnologies-web/OpsPilot/e2e/framework"
)

// RollbackScenario validates that OpsPilot can recover a service after a bad deploy:
//  1. Deploy the good variant → verify it's live
//  2. Push the broken variant → trigger a second deploy → wait for failure
//  3. Trigger rollback via API → wait for the service to return to "live"
//  4. Verify the live URL is reachable again
type RollbackScenario struct {
	base Base
	name string
}

func NewRollbackScenario(cfg *framework.Config, appKey string) *RollbackScenario {
	app := catalog.Catalog[appKey]
	if app == nil {
		panic(fmt.Sprintf("unknown app key: %s", appKey))
	}
	if app.BrokenVariant == nil {
		panic(fmt.Sprintf("app %s has no BrokenVariant", appKey))
	}
	return &RollbackScenario{
		base: NewBase(cfg, app),
		name: "rollback/" + appKey,
	}
}

func (s *RollbackScenario) ScenarioName() string { return s.name }
func (s *RollbackScenario) Framework() string     { return s.base.App.Framework }
func (s *RollbackScenario) Suite() string         { return "rollback" }

func (s *RollbackScenario) Run(ctx context.Context) *framework.ScenarioResult {
	pt := &framework.PhaseTracker{}
	logger := framework.NewLogger(s.name, s.base.Cfg.Verbose)
	defer s.base.Teardown(context.Background())

	// ── Phase 1: good deploy ──────────────────────────────────────────────────
	if err := s.base.Setup(ctx, pt, logger); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	dep, err := s.base.Deploy(ctx, pt, logger)
	if err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(),
			"good deploy failed: "+err.Error())
	}

	goodDeployID := ""
	if dep != nil {
		goodDeployID = dep.ID
	}

	var liveURL string
	if err := pt.Track("verify-good-deploy", func() error {
		health, err := s.base.Client.GetEnvHealth(ctx, s.base.projectID, s.base.envID)
		if err != nil {
			return fmt.Errorf("health: %w", err)
		}
		if health.URL == nil {
			return fmt.Errorf("no live URL after good deploy")
		}
		liveURL = *health.URL
		return s.base.Client.VerifyHTTP(ctx, liveURL, s.base.App.HealthPath)
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	logger.Logf("good deploy live at %s (deployID=%s)", liveURL, goodDeployID)

	// ── Phase 2: push broken variant + trigger bad deploy ─────────────────────
	if err := pt.Track("push-broken-variant", func() error {
		return s.base.Github.PushFiles(ctx,
			s.base.Cfg.GithubOwner,
			s.base.App.RepoName,
			s.base.App.BrokenVariant,
			"E2E: push broken variant for rollback test",
		)
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	// Wait a moment so GitHub has the new commit before we trigger deploy
	time.Sleep(3 * time.Second)

	badDep, badErr := s.base.Deploy(ctx, pt, logger)
	if badErr == nil && badDep != nil && badDep.Status == "live" {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(),
			"broken variant deployed successfully — broken files should have failed")
	}
	if badDep == nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(),
			"broken deploy produced no deployment record — cannot trigger rollback")
	}
	logger.Logf("broken deploy failed as expected (deployID=%s): %v", badDep.ID, badErr)

	// ── Phase 3: rollback ─────────────────────────────────────────────────────
	// Pass the failed deployment ID — the handler resolves the environment from it
	// and rolls back to the previous successful image.
	if err := pt.Track("trigger-rollback", func() error {
		return s.base.Client.Rollback(ctx, s.base.projectID, badDep.ID)
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), "rollback trigger: "+err.Error())
	}

	// Wait for the rollback deployment to complete
	rollbackCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	var rolledBackDep *framework.Deployment
	if err := pt.Track("wait-rollback-deploy", func() error {
		logger.Log("waiting for rollback deploy…")
		var e error
		rolledBackDep, e = s.base.Client.WaitForDeployment(rollbackCtx, s.base.projectID, logger.Log)
		return e
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), "rollback deploy failed: "+err.Error())
	}

	_ = rolledBackDep

	// ── Phase 4: verify service is live again ─────────────────────────────────
	if err := pt.Track("verify-rollback-health", func() error {
		health, err := s.base.Client.GetEnvHealth(ctx, s.base.projectID, s.base.envID)
		if err != nil {
			return fmt.Errorf("health after rollback: %w", err)
		}
		if health.URL == nil {
			return fmt.Errorf("no live URL after rollback")
		}
		liveURL = *health.URL

		var lastErr error
		for i := 0; i < 3; i++ {
			if lastErr = s.base.Client.VerifyHTTP(ctx, liveURL, s.base.App.HealthPath); lastErr == nil {
				return nil
			}
			logger.Logf("post-rollback health check attempt %d failed: %v", i+1, lastErr)
			time.Sleep(10 * time.Second)
		}
		return fmt.Errorf("HTTP health failed after rollback (%d attempts): %w", 3, lastErr)
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	logger.Logf("rollback succeeded — service live at %s", liveURL)
	return s.base.Passed(pt, s.name, s.Suite(), s.Framework(), liveURL)
}

// AllRollbackScenarios returns rollback tests for every app that has a BrokenVariant.
func AllRollbackScenarios(cfg *framework.Config) []framework.Scenario {
	var out []framework.Scenario
	for key, app := range catalog.Catalog {
		if app.ExpectSuccess && app.BrokenVariant != nil {
			out = append(out, NewRollbackScenario(cfg, key))
		}
	}
	return out
}
