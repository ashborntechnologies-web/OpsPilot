// Command runner is the CLI entry point for the OpsPilot E2E test suite.
//
// Usage:
//
//	export E2E_AUTH_TOKEN=... E2E_GITHUB_TOKEN=... E2E_AWS_ACCOUNT_ID=... E2E_AWS_ROLE_ARN=...
//	go run ./e2e/cmd/runner -suite=deploy
//	go run ./e2e/cmd/runner -suite=all -parallel=2 -verbose
//	go run ./e2e/cmd/runner -suite=deploy -filter=nodejs -cleanup=false -dry-run
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ashborntechnologies-web/OpsPilot/e2e/framework"
	"github.com/ashborntechnologies-web/OpsPilot/e2e/scenarios"
)

func main() {
	// ── Flags ──────────────────────────────────────────────────────────────────
	suiteFlag := flag.String("suite", "deploy",
		"Test suite to run: deploy | failure | rollback | diagnosis | all")
	parallelFlag := flag.Int("parallel", 0,
		"Max parallel scenarios (0 = use E2E_PARALLEL or default 2)")
	cleanupFlag := flag.Bool("cleanup", true,
		"Delete OpsPilot projects after each scenario (false = leave for inspection)")
	reportFlag := flag.String("report", "",
		"Directory to write JSON+text reports (empty = use E2E_REPORT_DIR or ./e2e-reports)")
	filterFlag := flag.String("filter", "",
		"Only run scenarios whose name, framework, or suite contains this string")
	dryRunFlag := flag.Bool("dry-run", false,
		"Print scenario names without executing them")
	verboseFlag := flag.Bool("verbose", false,
		"Print step-by-step progress for each scenario")

	flag.Parse()

	// ── Config ─────────────────────────────────────────────────────────────────
	cfg, err := framework.LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Flag overrides take precedence over environment variables
	if *parallelFlag > 0 {
		cfg.MaxParallel = *parallelFlag
	}
	if !*cleanupFlag {
		cfg.Cleanup = false
	}
	if *reportFlag != "" {
		cfg.ReportDir = *reportFlag
	}
	if *filterFlag != "" {
		cfg.Filter = *filterFlag
	}
	if *verboseFlag {
		cfg.Verbose = true
	}

	// ── Scenario selection ────────────────────────────────────────────────────
	suite := strings.ToLower(*suiteFlag)
	var allScenarios []framework.Scenario

	switch suite {
	case "deploy":
		allScenarios = scenarios.AllDeployScenarios(cfg)
	case "failure":
		allScenarios = scenarios.AllFailureScenarios(cfg)
	case "rollback":
		allScenarios = scenarios.AllRollbackScenarios(cfg)
	case "diagnosis":
		allScenarios = scenarios.AllDiagnosisScenarios(cfg)
	case "all":
		allScenarios = append(allScenarios, scenarios.AllDeployScenarios(cfg)...)
		allScenarios = append(allScenarios, scenarios.AllFailureScenarios(cfg)...)
		allScenarios = append(allScenarios, scenarios.AllRollbackScenarios(cfg)...)
		allScenarios = append(allScenarios, scenarios.AllDiagnosisScenarios(cfg)...)
	default:
		log.Fatalf("unknown suite %q — valid: deploy | failure | rollback | diagnosis | all", suite)
	}

	if len(allScenarios) == 0 {
		log.Printf("no scenarios found for suite=%q filter=%q", suite, cfg.Filter)
		os.Exit(0)
	}

	// ── Dry run ───────────────────────────────────────────────────────────────
	if *dryRunFlag {
		log.Printf("dry-run: %d scenario(s) selected:", len(allScenarios))
		for _, s := range allScenarios {
			log.Printf("  [%s/%s] %s", s.Suite(), s.Framework(), s.ScenarioName())
		}
		os.Exit(0)
	}

	// ── Execution ─────────────────────────────────────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runner := framework.NewRunner(cfg)
	report := runner.RunAll(ctx, allScenarios)

	// ── Report ────────────────────────────────────────────────────────────────
	writer := framework.NewWriter(cfg.ReportDir)
	if err := writer.Write(report); err != nil {
		log.Printf("warning: failed to write report: %v", err)
	}

	// ── Summary ───────────────────────────────────────────────────────────────
	log.Printf("══════════════════════════════════════════")
	log.Printf("  E2E Results: %d/%d passed (%.0f%%)  failed=%d  skipped=%d",
		report.Passed, report.Total, report.SuccessRate, report.Failed, report.Skipped)
	log.Printf("  Duration: %v", report.Duration.Round(1e9))
	log.Printf("══════════════════════════════════════════")

	if report.Failed > 0 {
		os.Exit(1)
	}
}
