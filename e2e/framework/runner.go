package framework

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Scenario is the contract every test scenario must satisfy.
type Scenario interface {
	// ScenarioName returns a unique, human-readable identifier.
	ScenarioName() string
	// Framework returns the app framework under test (e.g. "nodejs", "fastapi").
	Framework() string
	// Suite returns the test suite ("deploy", "failure", "rollback", "diagnosis").
	Suite() string
	// Run executes the scenario and returns a populated ScenarioResult.
	// Implementations must handle their own cleanup via deferred calls.
	Run(ctx context.Context) *ScenarioResult
}

// Runner executes a list of Scenarios in parallel with retry and reports results.
type Runner struct {
	cfg     *Config
	writer  *Writer
	verbose bool
}

func NewRunner(cfg *Config) *Runner {
	return &Runner{
		cfg:     cfg,
		writer:  NewWriter(cfg.ReportDir),
		verbose: cfg.Verbose,
	}
}

// RunAll executes all provided scenarios and returns the final Report.
func (r *Runner) RunAll(ctx context.Context, scenarios []Scenario) *Report {
	// Apply filter
	var filtered []Scenario
	for _, s := range scenarios {
		if r.cfg.Filter == "" || strings.Contains(s.ScenarioName(), r.cfg.Filter) ||
			strings.Contains(s.Framework(), r.cfg.Filter) || strings.Contains(s.Suite(), r.cfg.Filter) {
			filtered = append(filtered, s)
		}
	}

	report := &Report{
		StartedAt: time.Now(),
		Total:     len(filtered),
	}

	if len(filtered) == 0 {
		log.Printf("[runner] no scenarios match filter %q", r.cfg.Filter)
		report.FinishedAt = time.Now()
		return report
	}

	log.Printf("[runner] executing %d scenario(s) with parallelism=%d", len(filtered), r.cfg.MaxParallel)

	// Semaphore to cap concurrent AWS deployments
	sem := make(chan struct{}, r.cfg.MaxParallel)
	var mu sync.Mutex
	var wg sync.WaitGroup

	results := make([]ScenarioResult, len(filtered))

	for i, s := range filtered {
		wg.Add(1)
		go func(idx int, sc Scenario) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result := r.runWithRetry(ctx, sc)
			mu.Lock()
			results[idx] = *result
			mu.Unlock()
		}(i, s)
	}

	wg.Wait()

	report.FinishedAt = time.Now()
	report.Duration = report.FinishedAt.Sub(report.StartedAt)
	report.Scenarios = results

	for _, res := range results {
		switch res.Status {
		case "passed":
			report.Passed++
		case "failed":
			report.Failed++
		case "skipped":
			report.Skipped++
		}
	}
	if report.Total > 0 {
		report.SuccessRate = float64(report.Passed) / float64(report.Total) * 100
	}

	return report
}

func (r *Runner) runWithRetry(ctx context.Context, s Scenario) *ScenarioResult {
	var result *ScenarioResult
	for attempt := 0; attempt <= r.cfg.RetryCount; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt*30) * time.Second
			log.Printf("[runner] retrying %q (attempt %d/%d) after %v",
				s.ScenarioName(), attempt+1, r.cfg.RetryCount+1, wait)
			select {
			case <-ctx.Done():
				return &ScenarioResult{
					Name:      s.ScenarioName(),
					Framework: s.Framework(),
					Suite:     s.Suite(),
					Status:    "failed",
					FailReason: "context cancelled",
				}
			case <-time.After(wait):
			}
		}

		log.Printf("[runner] → %s [%s/%s]", s.ScenarioName(), s.Suite(), s.Framework())
		start := time.Now()

		result = s.Run(ctx)
		result.RetryCount = attempt
		result.Duration = time.Since(start)

		if result.Status == "passed" {
			log.Printf("[runner] ✓ %s (%v)", s.ScenarioName(), result.Duration.Round(time.Second))
			return result
		}

		// Don't retry on hard failures (broken app expected to fail, etc.)
		if isHardFailure(result.FailReason) {
			break
		}

		log.Printf("[runner] ✗ %s: %s", s.ScenarioName(), result.FailReason)
	}

	return result
}

// isHardFailure returns true for errors that should not be retried.
func isHardFailure(reason string) bool {
	for _, kw := range []string{
		"intentionally broken", "expected failure", "diagnosis empty",
		"rollback", "context cancelled",
	} {
		if strings.Contains(strings.ToLower(reason), kw) {
			return true
		}
	}
	return false
}

// ── Phase timing helper used by scenarios ─────────────────────────────────────

// PhaseTracker accumulates timing phases for a scenario result.
type PhaseTracker struct {
	phases []Phase
}

func (pt *PhaseTracker) Track(name string, fn func() error) error {
	start := time.Now()
	err := fn()
	ph := Phase{Name: name, Duration: time.Since(start)}
	if err != nil {
		ph.Err = err.Error()
	}
	pt.phases = append(pt.phases, ph)
	return err
}

func (pt *PhaseTracker) Phases() []Phase { return pt.phases }

// Logger writes timestamped lines to stdout (used by scenarios during long-running ops).
type Logger struct {
	prefix  string
	verbose bool
}

func NewLogger(name string, verbose bool) *Logger {
	return &Logger{prefix: fmt.Sprintf("[%s]", name), verbose: verbose}
}

func (l *Logger) Log(msg string) {
	if l.verbose {
		log.Printf("%s %s", l.prefix, msg)
	}
}

func (l *Logger) Logf(f string, a ...interface{}) {
	l.Log(fmt.Sprintf(f, a...))
}
