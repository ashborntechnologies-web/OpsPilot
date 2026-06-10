package prompts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMustResolveEnvWins(t *testing.T) {
	t.Setenv("TEST_PROMPT", "inline text")
	t.Setenv("TEST_PROMPT_FILE", "/nonexistent")
	if got := mustResolve("TEST_PROMPT", "TEST_PROMPT_FILE"); got != "inline text" {
		t.Errorf("got %q, want inline text", got)
	}
}

func TestMustResolveFileFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.txt")
	if err := os.WriteFile(path, []byte("file text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_PROMPT", "")
	t.Setenv("TEST_PROMPT_FILE", path)
	if got := mustResolve("TEST_PROMPT", "TEST_PROMPT_FILE"); got != "file text" {
		t.Errorf("got %q, want file text", got)
	}
}

func TestMustResolvePanicsWhenUnset(t *testing.T) {
	t.Setenv("TEST_PROMPT", "")
	t.Setenv("TEST_PROMPT_FILE", "")
	defer func() {
		if recover() == nil {
			t.Error("expected panic when no prompt source is configured")
		}
	}()
	mustResolve("TEST_PROMPT", "TEST_PROMPT_FILE")
}
