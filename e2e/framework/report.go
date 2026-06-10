package framework

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Phase tracks timing for each major step within a scenario.
type Phase struct {
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration_ms"`
	Err      string        `json:"error,omitempty"`
}

// ScenarioResult captures the full outcome of one E2E scenario.
type ScenarioResult struct {
	Name        string    `json:"name"`
	Framework   string    `json:"framework"`
	Suite       string    `json:"suite"`
	Status      string    `json:"status"` // "passed" | "failed" | "skipped"
	StartedAt   time.Time `json:"started_at"`
	Duration    time.Duration `json:"duration_ms"`
	Phases      []Phase   `json:"phases"`
	LiveURL     string    `json:"live_url,omitempty"`
	Diagnosis   string    `json:"diagnosis,omitempty"`
	FailReason  string    `json:"fail_reason,omitempty"`
	RetryCount  int       `json:"retry_count"`
	ProjectID   string    `json:"project_id,omitempty"`
}

// Report aggregates all scenario results for a full run.
type Report struct {
	StartedAt    time.Time       `json:"started_at"`
	FinishedAt   time.Time       `json:"finished_at"`
	Duration     time.Duration   `json:"duration_ms"`
	Total        int             `json:"total"`
	Passed       int             `json:"passed"`
	Failed       int             `json:"failed"`
	Skipped      int             `json:"skipped"`
	SuccessRate  float64         `json:"success_rate_pct"`
	Scenarios    []ScenarioResult `json:"scenarios"`
	Regressions  []string        `json:"regressions,omitempty"`
}

// Writer serialises a Report to disk and optionally prints a summary.
type Writer struct {
	dir string
}

func NewWriter(dir string) *Writer {
	os.MkdirAll(dir, 0755)
	return &Writer{dir: dir}
}

func (w *Writer) Write(r *Report) error {
	stamp := r.StartedAt.Format("20060102-150405")

	// JSON
	jsonPath := filepath.Join(w.dir, "report-"+stamp+".json")
	f, err := os.Create(jsonPath)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return err
	}

	// Human-readable text
	txtPath := filepath.Join(w.dir, "report-"+stamp+".txt")
	tf, err := os.Create(txtPath)
	if err != nil {
		return err
	}
	defer tf.Close()
	fmt.Fprintln(tf, w.formatText(r))

	return nil
}

func (w *Writer) PrintSummary(r *Report) {
	fmt.Println(w.formatText(r))
}

func (w *Writer) formatText(r *Report) string {
	var sb strings.Builder

	line := func(f string, a ...interface{}) { fmt.Fprintf(&sb, f+"\n", a...) }
	sep := func() { line(strings.Repeat("─", 72)) }

	sep()
	line("  OpsPilot E2E Test Report — %s", r.StartedAt.Format(time.RFC1123))
	sep()
	line("  Duration: %v   Total: %d   ✓ %d   ✗ %d   ○ %d   Success: %.1f%%",
		r.Duration.Round(time.Second), r.Total, r.Passed, r.Failed, r.Skipped, r.SuccessRate)
	sep()

	for _, s := range r.Scenarios {
		icon := "✓"
		if s.Status == "failed" {
			icon = "✗"
		} else if s.Status == "skipped" {
			icon = "○"
		}
		line("  %s  %-40s  %s  %v", icon, s.Name, s.Framework, s.Duration.Round(time.Second))
		if s.LiveURL != "" {
			line("     url: %s", s.LiveURL)
		}
		if s.FailReason != "" {
			line("     fail: %s", s.FailReason)
		}
		for _, ph := range s.Phases {
			phIcon := "·"
			if ph.Err != "" {
				phIcon = "!"
			}
			line("     %s %-20s %v", phIcon, ph.Name, ph.Duration.Round(time.Second))
			if ph.Err != "" {
				line("       └─ %s", ph.Err)
			}
		}
	}

	if len(r.Regressions) > 0 {
		sep()
		line("  REGRESSIONS DETECTED:")
		for _, reg := range r.Regressions {
			line("    ! %s", reg)
		}
	}

	sep()
	return sb.String()
}
