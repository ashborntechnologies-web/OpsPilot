package scenarios

import (
	"context"
	"fmt"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/e2e/catalog"
	"github.com/ashborntechnologies-web/OpsPilot/e2e/framework"
)

// DeployScenario verifies the happy-path deployment flow for a single app.
//
//  1. Push app files to GitHub
//  2. Create project + environment → provision AWS infrastructure
//  3. Trigger deploy → wait for "live"
//  4. Verify HTTP health endpoint returns 200
//  5. Validate deployment log entries exist
type DeployScenario struct {
	base Base
	name string
}

func NewDeployScenario(cfg *framework.Config, appKey string) *DeployScenario {
	app := catalog.Catalog[appKey]
	if app == nil {
		panic(fmt.Sprintf("unknown app key: %s", appKey))
	}
	return &DeployScenario{
		base: NewBase(cfg, app),
		name: "deploy/" + appKey,
	}
}

func (s *DeployScenario) ScenarioName() string { return s.name }
func (s *DeployScenario) Framework() string     { return s.base.App.Framework }
func (s *DeployScenario) Suite() string         { return "deploy" }

func (s *DeployScenario) Run(ctx context.Context) *framework.ScenarioResult {
	pt := &framework.PhaseTracker{}
	logger := framework.NewLogger(s.name, s.base.Cfg.Verbose)
	defer s.base.Teardown(context.Background())

	// ── Setup (provision) ─────────────────────────────────────────────────────
	if err := s.base.Setup(ctx, pt, logger); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	// ── Deploy ────────────────────────────────────────────────────────────────
	dep, deployErr := s.base.Deploy(ctx, pt, logger)
	if deployErr != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), deployErr.Error())
	}

	// ── Verify HTTP health ────────────────────────────────────────────────────
	var liveURL string
	if err := pt.Track("verify-health", func() error {
		health, err := s.base.Client.GetEnvHealth(ctx, s.base.projectID, s.base.envID)
		if err != nil {
			return fmt.Errorf("health API: %w", err)
		}
		if health.URL == nil {
			return fmt.Errorf("no live URL in health response")
		}
		liveURL = *health.URL

		// Retry HTTP check up to 3× — ALB takes a few seconds to propagate
		var lastErr error
		for i := 0; i < 3; i++ {
			if lastErr = s.base.Client.VerifyHTTP(ctx, liveURL, s.base.App.HealthPath); lastErr == nil {
				return nil
			}
			logger.Logf("health check attempt %d failed: %v", i+1, lastErr)
			time.Sleep(10 * time.Second)
		}
		return fmt.Errorf("HTTP health failed after 3 attempts: %w", lastErr)
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	// ── Validate deployment log lines exist ───────────────────────────────────
	pt.Track("check-logs", func() error {
		lines, err := s.base.Client.GetLogs(ctx, s.base.projectID, s.base.envID, 50)
		if err != nil {
			return fmt.Errorf("fetch logs: %w", err)
		}
		if len(lines) == 0 {
			return fmt.Errorf("no log lines returned")
		}
		logger.Logf("got %d log lines", len(lines))
		return nil
	})

	// ── Deployment status transition check ────────────────────────────────────
	if err := pt.Track("check-deployment-record", func() error {
		if dep == nil {
			return fmt.Errorf("no deployment record")
		}
		if dep.Status != "live" {
			return fmt.Errorf("expected status=live, got %s", dep.Status)
		}
		return nil
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	return s.base.Passed(pt, s.name, s.Suite(), s.Framework(), liveURL)
}

// AllDeployScenarios returns the happy-path suite for all apps that expect success.
func AllDeployScenarios(cfg *framework.Config) []framework.Scenario {
	var out []framework.Scenario
	for key, app := range catalog.Catalog {
		if app.ExpectSuccess {
			out = append(out, NewDeployScenario(cfg, key))
		}
	}
	return out
}
