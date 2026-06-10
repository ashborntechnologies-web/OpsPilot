// Package prompts loads the platform's AI prompts from the environment at startup.
// The prompt texts are trade secrets and are deliberately NOT embedded in source:
// each prompt is read from an env var, or from a file referenced by a *_FILE env
// var. MustLoad panics when a prompt has no configured source.
package prompts

import (
	"fmt"
	"os"
	"strings"
)

var (
	intentClassifier string
	diagnosis        string
	loaded           bool
)

// MustLoad resolves both prompts. Call once at startup, after .env is loaded.
// Panics with a setup-instruction message when a prompt is missing — the
// platform cannot classify intents or diagnose failures without them.
func MustLoad() {
	intentClassifier = mustResolve("INTENT_CLASSIFIER_PROMPT", "INTENT_CLASSIFIER_PROMPT_FILE")
	diagnosis = mustResolve("DIAGNOSIS_PROMPT", "DIAGNOSIS_PROMPT_FILE")
	loaded = true
}

// IntentClassifier returns the intent-classification system prompt.
func IntentClassifier() string {
	mustBeLoaded()
	return intentClassifier
}

// Diagnosis returns the deployment-diagnosis system prompt.
func Diagnosis() string {
	mustBeLoaded()
	return diagnosis
}

func mustBeLoaded() {
	if !loaded {
		panic("prompts.MustLoad() was not called at startup")
	}
}

func mustResolve(envKey, fileKey string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	if path := strings.TrimSpace(os.Getenv(fileKey)); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			panic(fmt.Sprintf("prompts: %s points to %q but the file could not be read: %v", fileKey, path, err))
		}
		if text := strings.TrimSpace(string(b)); text != "" {
			return text
		}
		panic(fmt.Sprintf("prompts: %s points to %q but the file is empty", fileKey, path))
	}
	panic(fmt.Sprintf(
		"prompts: no source configured for the %s prompt — set %s (inline text) or %s (path to a prompt file). "+
			"Prompt texts are trade secrets and are not embedded in the binary; see .env.example.",
		strings.ToLower(strings.TrimSuffix(envKey, "_PROMPT")), envKey, fileKey,
	))
}
