package diagnosis

import (
	"strings"
	"testing"
)

func TestBuildDiagnosisContext_WithLogs(t *testing.T) {
	out := buildDiagnosisContext(
		"build failed: exit 1",
		[]string{"line one", "line two"},
		"deploy history here",
		"past incidents here",
		"2026-01-01 [error] build.failed",
	)

	for _, want := range []string{
		"## Failure Reason", "build failed: exit 1",
		"## Application Logs", "line one", "line two",
		"## Event Timeline", "build.failed",
		"## Deployment History", "deploy history here",
		"## Past Incidents", "past incidents here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("context missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuildDiagnosisContext_NoLogs(t *testing.T) {
	out := buildDiagnosisContext("reason", nil, "h", "p", "")
	if !strings.Contains(out, "No application logs available") {
		t.Errorf("expected graceful no-logs message, got:\n%s", out)
	}
	if !strings.Contains(out, "No event timeline available") {
		t.Errorf("expected graceful no-timeline message, got:\n%s", out)
	}
}

func TestBuildDiagnosisContext_EmptyFailureReason(t *testing.T) {
	out := buildDiagnosisContext("   ", []string{"x"}, "h", "p", "t")
	if !strings.Contains(out, "No recorded failure reason.") {
		t.Errorf("expected fallback failure-reason message, got:\n%s", out)
	}
}

func TestBuildDiagnosisContext_TruncatesLargeLogs(t *testing.T) {
	var lines []string
	lines = append(lines, "FIRST_LINE_SENTINEL")
	for i := 0; i < 400; i++ {
		lines = append(lines, "padding-line-with-some-length-to-exceed-the-cap")
	}
	lines = append(lines, "LAST_LINE_SENTINEL")

	out := buildDiagnosisContext("reason", lines, "h", "p", "t")

	if !strings.Contains(out, "(truncated)") {
		t.Errorf("expected truncation marker for oversized logs")
	}
	if strings.Contains(out, "FIRST_LINE_SENTINEL") {
		t.Errorf("oldest line should have been truncated away")
	}
	if !strings.Contains(out, "LAST_LINE_SENTINEL") {
		t.Errorf("most recent line should be retained")
	}
}
