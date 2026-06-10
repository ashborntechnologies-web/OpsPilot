package scenarios

import (
	"context"
	"fmt"

	"github.com/ashborntechnologies-web/OpsPilot/e2e/catalog"
	"github.com/ashborntechnologies-web/OpsPilot/e2e/framework"
)

// DiagnosisScenario validates OpsPilot's AI diagnosis feature:
//  1. Deploy a broken app so there is a failed deployment
//  2. Call DiagnoseDeployment on the latest failed deployment
//  3. Assert that the response contains a non-empty, human-readable explanation
type DiagnosisScenario struct {
	base Base
	name string
}

func NewDiagnosisScenario(cfg *framework.Config, appKey string) *DiagnosisScenario {
	app := catalog.Catalog[appKey]
	if app == nil {
		panic(fmt.Sprintf("unknown app key: %s", appKey))
	}
	return &DiagnosisScenario{
		base: NewBase(cfg, app),
		name: "diagnosis/" + appKey,
	}
}

func (s *DiagnosisScenario) ScenarioName() string { return s.name }
func (s *DiagnosisScenario) Framework() string     { return s.base.App.Framework }
func (s *DiagnosisScenario) Suite() string         { return "diagnosis" }

func (s *DiagnosisScenario) Run(ctx context.Context) *framework.ScenarioResult {
	pt := &framework.PhaseTracker{}
	logger := framework.NewLogger(s.name, s.base.Cfg.Verbose)
	defer s.base.Teardown(context.Background())

	// ── Setup: provision the environment ─────────────────────────────────────
	if err := s.base.Setup(ctx, pt, logger); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	// ── Deploy broken app → expect failure ───────────────────────────────────
	dep, deployErr := s.base.Deploy(ctx, pt, logger)
	if deployErr == nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(),
			fmt.Sprintf("expected deploy to fail but got status=%s", dep.Status))
	}

	if dep == nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(),
			"deploy failed before a deployment record was created — nothing to diagnose")
	}
	logger.Logf("deploy failed as expected (deployID=%s): %v", dep.ID, deployErr)

	// ── Call AI diagnosis ─────────────────────────────────────────────────────
	// Pass the failed deployment ID — the diagnosis endpoint reads its logs and
	// uses Claude to explain the failure.
	var diagnosis string
	if err := pt.Track("diagnose", func() error {
		logger.Log("calling DiagnoseDeployment…")
		var err error
		diagnosis, err = s.base.Client.DiagnoseDeployment(ctx, s.base.projectID, dep.ID)
		return err
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(),
			"diagnosis API error: "+err.Error())
	}

	// ── Validate the diagnosis is non-trivial ─────────────────────────────────
	if err := pt.Track("validate-diagnosis", func() error {
		if diagnosis == "" {
			return fmt.Errorf("diagnosis response was empty")
		}
		if len(diagnosis) < 50 {
			return fmt.Errorf("diagnosis too short (%d chars) — likely a stub: %q", len(diagnosis), diagnosis)
		}
		logger.Logf("diagnosis (%d chars): %.200s…", len(diagnosis), diagnosis)
		return nil
	}); err != nil {
		return s.base.Failed(pt, s.name, s.Suite(), s.Framework(), err.Error())
	}

	return s.base.Passed(pt, s.name, s.Suite(), s.Framework(), "")
}

// AllDiagnosisScenarios returns diagnosis tests for all broken-runtime apps
// (build failures don't produce ECS logs, so they're less useful for AI diagnosis).
func AllDiagnosisScenarios(cfg *framework.Config) []framework.Scenario {
	// Only apps that fail at runtime produce the log output that diagnosis works best with
	keys := []string{"broken-runtime", "broken-missing-env"}
	out := make([]framework.Scenario, 0, len(keys))
	for _, k := range keys {
		if catalog.Catalog[k] != nil {
			out = append(out, NewDiagnosisScenario(cfg, k))
		}
	}
	return out
}
