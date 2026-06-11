package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/ashborntechnologies-web/OpsPilot/internal/testutil"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
)

func TestContentKeyNormalizes(t *testing.T) {
	a := contentKey("When seeing  \"OOM\",   the fix was: raise memory")
	b := contentKey("when seeing \"oom\", the fix was: raise   memory")
	if a != b {
		t.Errorf("normalized keys should match: %q vs %q", a, b)
	}
	long := contentKey(strings.Repeat("x", 500))
	if len(long) != 120 {
		t.Errorf("key should truncate to 120 chars, got %d", len(long))
	}
}

func TestFormatForPrompt(t *testing.T) {
	if FormatForPrompt(nil) != "" {
		t.Error("empty memory should render as empty string")
	}
	out := FormatForPrompt([]models.ProjectMemory{
		{MemoryType: models.MemorySuccessfulFix, Content: "fix A"},
		{MemoryType: models.MemoryDeployPattern, Content: "pattern B"},
	})
	if !strings.Contains(out, "## Project Memory") ||
		!strings.Contains(out, "[successful_fix] fix A") ||
		!strings.Contains(out, "[deploy_pattern] pattern B") {
		t.Errorf("unexpected prompt rendering:\n%s", out)
	}
}

// TestGetRelevantMemoryRanking is a DB integration test (skipped without TEST_DATABASE_URL).
func TestGetRelevantMemoryRanking(t *testing.T) {
	db := testutil.NewTestDB(t)
	svc := NewService(db, nil)
	ctx := context.Background()

	userID := testutil.CreateUser(t, db)
	projectID := testutil.CreateProject(t, db, userID)

	// Low-signal memory: low confidence, single reference.
	if err := svc.upsert(ctx, projectID, models.MemoryRecurringFailure, "rarely seen failure", models.MemorySourcePatternDetected, 0.2); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// High-signal memory: confirmed fix, referenced repeatedly.
	for i := 0; i < 3; i++ {
		if err := svc.upsert(ctx, projectID, models.MemorySuccessfulFix, "When seeing X, the fix was Y", models.MemorySourceUserConfirmed, 1.0); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	memories, err := svc.GetRelevantMemory(ctx, projectID, 5)
	if err != nil {
		t.Fatalf("GetRelevantMemory: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("got %d memories, want 2 (near-duplicates must collapse)", len(memories))
	}
	if memories[0].MemoryType != models.MemorySuccessfulFix {
		t.Errorf("highest-signal memory should rank first, got %s", memories[0].MemoryType)
	}
	if memories[0].ReferenceCount != 3 {
		t.Errorf("reference_count = %d, want 3", memories[0].ReferenceCount)
	}
}
