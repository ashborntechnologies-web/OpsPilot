package monitor

import (
	"strings"
	"testing"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
)

func TestMapEventToAlert(t *testing.T) {
	tests := []struct {
		eventType string
		payload   map[string]any
		want      string
	}{
		{models.EventRuntimeServiceDown, nil, models.AlertTypeServiceDown},
		{models.EventRuntimeTasksDegraded, nil, models.AlertTypeTasksDegraded},
		{models.EventRuntimeHighErrorRate, nil, models.AlertTypeHighErrorRate},
		{models.EventRuntimeHighLatency, nil, models.AlertTypeHighLatency},
		{models.EventRuntimeLogAnomaly, map[string]any{"pattern_type": PatternCrashLoop}, models.AlertTypeCrashLoop},
		{models.EventRuntimeLogAnomaly, map[string]any{"pattern_type": PatternException}, models.AlertTypeLogAnomaly},
		{models.EventDeploymentStuck, nil, models.AlertTypeDeployStuck},
		{models.EventDeployStarted, nil, ""},
		{models.EventRuntimeServiceRecovered, nil, ""},
	}
	for _, tt := range tests {
		if got := MapEventToAlert(tt.eventType, tt.payload); got != tt.want {
			t.Errorf("MapEventToAlert(%q) = %q, want %q", tt.eventType, got, tt.want)
		}
	}
}

func TestAlertTitle(t *testing.T) {
	if got := alertTitle(models.AlertTypeServiceDown, "production"); got != "Service down — production" {
		t.Errorf("unexpected title: %q", got)
	}
	if got := alertTitle("custom_type", ""); got != "custom_type" {
		t.Errorf("unknown types should fall back to the raw type, got %q", got)
	}
}

func TestScanLinesDetectsPatterns(t *testing.T) {
	lines := []string{
		"INFO request handled in 12ms",
		"Traceback (most recent call last):",
		"  File \"app.py\", line 10",
		"ValueError: bad input",
		"ECONNREFUSED connecting to db:5432",
		"worker killed: out of memory",
		"could not resolve: no such host redis.internal",
	}
	matches := ScanLines(lines)

	got := map[string]anomalyMatch{}
	for _, m := range matches {
		got[m.PatternType] = m
	}

	for _, want := range []string{PatternException, PatternConnectionFailure, PatternOOM, PatternDependencyFailure} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected pattern %q to be detected; got %v", want, keys(got))
		}
	}
	if got[PatternOOM].Severity != models.SeverityError {
		t.Errorf("OOM should be error severity")
	}
	if got[PatternException].Severity != models.SeverityWarn {
		t.Errorf("exception should be warn severity")
	}
}

func TestScanLinesCrashLoop(t *testing.T) {
	var lines []string
	for i := 0; i < 4; i++ {
		lines = append(lines, "ERROR failed to bind port 8080: address already in use")
		lines = append(lines, "restarting...")
	}
	matches := ScanLines(lines)

	found := false
	for _, m := range matches {
		if m.PatternType == PatternCrashLoop {
			found = true
			if m.LineCount < 3 {
				t.Errorf("crash loop line_count = %d, want >= 3", m.LineCount)
			}
			if m.Severity != models.SeverityError {
				t.Errorf("crash loop should be error severity")
			}
		}
	}
	if !found {
		t.Fatal("expected crash loop to be detected")
	}
}

func TestScanLinesCleanLogs(t *testing.T) {
	lines := []string{
		"INFO server started on :8080",
		"GET /health 200 1ms",
		"GET / 200 14ms",
	}
	if matches := ScanLines(lines); len(matches) != 0 {
		t.Errorf("clean logs should produce no anomalies, got %v", matches)
	}
}

func TestScanLinesSampleCapped(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "java.lang.Exception: boom "+strings.Repeat("x", i))
	}
	for _, m := range ScanLines(lines) {
		if len(m.MatchedLines) > 5 {
			t.Errorf("matched_lines sample should be capped at 5, got %d", len(m.MatchedLines))
		}
	}
}

func keys(m map[string]anomalyMatch) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
