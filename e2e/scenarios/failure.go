package scenarios

import (
	"context"
	"fmt"
	"strings"

	"github.com/ashborntechnologies-web/OpsPilot/e2e/catalog"
	"github.com/ashborntechnologies-web/OpsPilot/e2e/framework"
)

// FailureScenario verifies that OpsPilot handles broken applications correctly:
// the deployment reaches "failed" status, the failure_reason is populated,
// and the OpsPilot API correctly surfaces the error.
type FailureScenario struct {
	base Base
	name string
}

func NewFailureScenario(cfg *framework.Config, appKey string) *FailureScenario {
	app := catalog.Catalog[appKey]
	if app == nil {
		panic(fmt.Sprintf("unknown app key: %s", appKey))
	}
	return &FailureScenario{
		base: NewBase(cfg, app),
		name: "failure/" + appKey,
	}
}

func (s *FailureScenario) ScenarioName() string { return s.name }
func (s *FailureScenario) Framework() string     { return s.base.App.Framework }
func (s *FailureScenario) Suite() string         { return "failure" }

func (s *FailureScenario) Run(ctx context.Context) *framework.ScenarioResult {
	pt := &framework.PhaseTracker{}
	logger := framework.NewLogger(s.name, s.base.Cfg.Verbose)
	defer s.base.Teardown(context.Background())

	if err := s.base.Setup(ctx, pt, logger); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	// Trigger deploy — we EXPECT this to fail
	dep, deployErr := s.base.Deploy(ctx, pt, logger)

	if deployErr == nil {
		// Deploy succeeded but it should have failed
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(),
			fmt.Sprintf("expected deploy to fail but it succeeded (status: %s)", dep.Status))
	}

	// Verify the failure reason contains the expected keyword
	if err := pt.Track("verify-failure-reason", func() error {
		if dep == nil {
			return nil // failure happened before a deployment record was created (e.g. build error)
		}
		reason := ""
		if dep.FailureReason != nil {
			reason = *dep.FailureReason
		}
		kw := s.base.App.ExpectedFailureKeyword
		if kw != "" && !strings.Contains(strings.ToLower(reason), strings.ToLower(kw)) {
			return fmt.Errorf("failure_reason %q does not contain expected keyword %q", reason, kw)
		}
		logger.Logf("confirmed failure reason: %s", reason)
		return nil
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	// Verify health endpoint reflects "not deployed" or "down" state
	pt.Track("verify-health-reflects-failure", func() error {
		health, err := s.base.Client.GetEnvHealth(ctx, s.base.projectID, s.base.envID)
		if err != nil {
			return nil // health endpoint may 404 if ECS service was never created
		}
		if health.Status == "up" {
			return fmt.Errorf("health says 'up' but deployment failed")
		}
		return nil
	})

	// All checks passed — the failure scenario behaved correctly
	result := s.base.Passed(pt, s.name, s.Suite(), s.Framework(), "")
	result.FailReason = fmt.Sprintf("intentionally broken: %s", deployErr.Error())
	return result
}

// AllFailureScenarios returns the failure suite for all intentionally broken apps.
func AllFailureScenarios(cfg *framework.Config) []framework.Scenario {
	var out []framework.Scenario
	for key, app := range catalog.Catalog {
		if !app.ExpectSuccess {
			out = append(out, NewFailureScenario(cfg, key))
		}
	}
	return out
}
