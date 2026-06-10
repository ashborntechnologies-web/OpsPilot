package framework

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all external dependencies for the E2E framework.
// Every value is read from environment variables so CI can inject them
// without touching source files.
type Config struct {
	// OpsPilot backend URL (e.g. http://localhost:8080)
	OpsPilotURL string
	// Clerk JWT for the dedicated E2E service account
	AuthToken string

	// GitHub personal access token with repo + admin:repo_hook scopes
	GithubToken string
	// GitHub user or org that owns the test repos (e.g. "convdeploy-e2e")
	GithubOwner string

	// Dedicated testing AWS account — separate from production
	AWSAccountID string
	// Pre-deployed ConvDeployPlatformRole ARN in the test account
	AWSRoleARN string
	// STS external ID embedded in the test account's role trust policy
	AWSExternalID string
	// Region for test infrastructure
	AWSRegion string

	// Max concurrent deployment tests
	MaxParallel int
	// Max retry attempts per scenario on infrastructure errors
	RetryCount int
	// Whether to delete all AWS resources after each test
	Cleanup bool
	// Directory for JSON / HTML reports
	ReportDir string
	// Only run scenarios whose names contain this substring (empty = all)
	Filter string
	// Log every step even on success
	Verbose bool
}

func LoadConfig() (*Config, error) {
	c := &Config{
		OpsPilotURL:  envOr("E2E_OPSPILOT_URL", "http://localhost:8080"),
		AuthToken:    os.Getenv("E2E_AUTH_TOKEN"),
		GithubToken:  os.Getenv("E2E_GITHUB_TOKEN"),
		GithubOwner:  envOr("E2E_GITHUB_OWNER", "convdeploy-e2e"),
		AWSAccountID: os.Getenv("E2E_AWS_ACCOUNT_ID"),
		AWSRoleARN:   os.Getenv("E2E_AWS_ROLE_ARN"),
		AWSExternalID: os.Getenv("E2E_AWS_EXTERNAL_ID"),
		AWSRegion:    envOr("E2E_AWS_REGION", "us-east-1"),
		MaxParallel:  envInt("E2E_PARALLEL", 3),
		RetryCount:   envInt("E2E_RETRY", 1),
		Cleanup:      envBool("E2E_CLEANUP", true),
		ReportDir:    envOr("E2E_REPORT_DIR", "./e2e-reports"),
		Filter:       os.Getenv("E2E_FILTER"),
		Verbose:      envBool("E2E_VERBOSE", false),
	}

	var missing []string
	if c.AuthToken == "" {
		missing = append(missing, "E2E_AUTH_TOKEN")
	}
	if c.GithubToken == "" {
		missing = append(missing, "E2E_GITHUB_TOKEN")
	}
	if c.AWSAccountID == "" {
		missing = append(missing, "E2E_AWS_ACCOUNT_ID")
	}
	if c.AWSRoleARN == "" {
		missing = append(missing, "E2E_AWS_ROLE_ARN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %v", missing)
	}

	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}
