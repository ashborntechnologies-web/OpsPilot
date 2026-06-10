// Package scenarios implements each E2E test suite as runnable Scenario values.
package scenarios

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/e2e/catalog"
	"github.com/ashborntechnologies-web/OpsPilot/e2e/framework"
)

// Base contains shared setup/teardown logic used by every scenario.
// Scenarios embed Base and call base.Setup() at the start of Run()
// and base.Teardown() via defer.
type Base struct {
	Cfg    *framework.Config
	Client *framework.Client
	Github *framework.GithubClient
	App    *catalog.TestApp

	// Filled in by Setup
	accountID string
	projectID string
	envID     string
	envName   string

	repoCreated bool
}

func NewBase(cfg *framework.Config, app *catalog.TestApp) Base {
	return Base{
		Cfg:    cfg,
		Client: framework.NewClient(cfg.OpsPilotURL, cfg.AuthToken),
		Github: framework.NewGithubClient(cfg.GithubToken),
		App:    app,
		envName: "staging",
	}
}

// EnsureAWSAccount returns the ID of a connected AWS account for the test user.
// It reuses an existing account with the same role ARN if one exists, to avoid
// re-connecting on every test run.
func (b *Base) EnsureAWSAccount(ctx context.Context) (string, error) {
	accounts, err := b.Client.ListAWSAccounts(ctx)
	if err != nil {
		return "", fmt.Errorf("list accounts: %w", err)
	}
	for _, a := range accounts {
		if a.IAMRoleARN == b.Cfg.AWSRoleARN {
			return a.ID, nil
		}
	}
	// Connect it
	account, err := b.Client.ConnectAWSAccount(ctx,
		"E2E Test Account",
		b.Cfg.AWSAccountID,
		b.Cfg.AWSRoleARN,
		b.Cfg.AWSExternalID,
	)
	if err != nil {
		return "", fmt.Errorf("connect aws account: %w", err)
	}
	return account.ID, nil
}

// EnsureRepo pushes the app's file tree to GitHub, creating the repo if needed.
func (b *Base) EnsureRepo(ctx context.Context, files map[string]string, commitMsg string) (string, error) {
	repoURL, err := b.Github.EnsureRepo(ctx, b.Cfg.GithubOwner, b.App.RepoName, b.App.Description)
	if err != nil {
		return "", fmt.Errorf("ensure repo: %w", err)
	}
	b.repoCreated = true

	if err := b.Github.PushFiles(ctx, b.Cfg.GithubOwner, b.App.RepoName, files, commitMsg); err != nil {
		return "", fmt.Errorf("push files: %w", err)
	}
	return repoURL, nil
}

// Setup creates the GitHub repo, OpsPilot project, environment, and waits for provisioning.
// Returns a phase-tracked error.
func (b *Base) Setup(ctx context.Context, pt *framework.PhaseTracker, logger *framework.Logger) error {
	// 1. AWS account
	var err error
	if err = pt.Track("connect-aws", func() error {
		b.accountID, err = b.EnsureAWSAccount(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("setup connect-aws: %w", err)
	}

	// 2. GitHub repo
	accountIDStr := b.accountID
	if err = pt.Track("push-repo", func() error {
		branch, _ := b.Github.GetDefaultBranch(ctx, b.Cfg.GithubOwner, b.App.RepoName)
		_, err2 := b.EnsureRepo(ctx, b.App.Files, "E2E: initial commit")
		_ = branch
		return err2
	}); err != nil {
		return fmt.Errorf("setup push-repo: %w", err)
	}

	// 3. Create project
	if err = pt.Track("create-project", func() error {
		branch, _ := b.Github.GetDefaultBranch(ctx, b.Cfg.GithubOwner, b.App.RepoName)
		proj, err2 := b.Client.CreateProject(ctx, framework.CreateProjectRequest{
			Name:      b.App.RepoName + "-" + fmt.Sprintf("%d", time.Now().Unix()),
			RepoURL:   fmt.Sprintf("https://github.com/%s/%s", b.Cfg.GithubOwner, b.App.RepoName),
			RepoOwner: b.Cfg.GithubOwner,
			RepoName:  b.App.RepoName,
			Framework: b.App.Framework,
			Branch:    &branch,
			AccountID: &accountIDStr,
		})
		if err2 != nil {
			return err2
		}
		b.projectID = proj.ID
		return nil
	}); err != nil {
		return fmt.Errorf("setup create-project: %w", err)
	}

	// 4. Set env vars (if any)
	if err = pt.Track("set-env-vars", func() error {
		// Env vars are set after environment creation; we create env first
		return nil
	}); err != nil {
		return err
	}

	// 5. Create environment
	if err = pt.Track("create-environment", func() error {
		env, err2 := b.Client.CreateEnvironment(ctx, b.projectID, b.envName, b.Cfg.AWSRegion)
		if err2 != nil {
			return err2
		}
		b.envID = env.ID
		return nil
	}); err != nil {
		return fmt.Errorf("setup create-environment: %w", err)
	}

	// 6. Set env vars on the environment
	for k, v := range b.App.EnvVars {
		key, val := k, v
		pt.Track("set-env-var:"+key, func() error {
			return b.Client.UpsertEnvVar(ctx, b.projectID, b.envID, key, val, false)
		})
	}

	// 7. Wait for provisioning
	provisionCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	if err = pt.Track("provision", func() error {
		logger.Log("waiting for platform + project stacks…")
		_, err2 := b.Client.WaitForEnvironment(provisionCtx, b.projectID, b.envID, logger.Log)
		return err2
	}); err != nil {
		return fmt.Errorf("setup provision: %w", err)
	}

	return nil
}

// Deploy triggers a deploy and waits for it to reach a terminal state.
func (b *Base) Deploy(ctx context.Context, pt *framework.PhaseTracker, logger *framework.Logger) (*framework.Deployment, error) {
	var dep *framework.Deployment

	if err := pt.Track("trigger-deploy", func() error {
		return b.Client.TriggerDeployByEnvID(ctx, b.projectID, b.envID, b.envName)
	}); err != nil {
		return nil, err
	}

	deployCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	var deployErr error
	pt.Track("wait-deploy", func() error {
		logger.Log("waiting for CodeBuild + ECS rollout…")
		dep, deployErr = b.Client.WaitForDeployment(deployCtx, b.projectID, logger.Log)
		return deployErr
	})

	return dep, deployErr
}

// Teardown deletes the OpsPilot project (triggers background AWS cleanup) and optionally
// deletes the GitHub test repo.
func (b *Base) Teardown(ctx context.Context) {
	if !b.Cfg.Cleanup {
		return
	}

	tCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if b.projectID != "" {
		if err := b.Client.DeleteProject(tCtx, b.projectID); err != nil {
			log.Printf("[teardown] delete project %s: %v", b.projectID, err)
		}
	}
	// Note: we intentionally do NOT delete the GitHub test repo between runs —
	// the next run will reuse it and push updated files. Use E2E_CLEANUP_REPOS=true
	// for a full teardown (implemented in the runner's post-run hook).
}

// Passed is a convenience builder for a successful result.
func (b *Base) Passed(pt *framework.PhaseTracker, name, suite, fw, liveURL string) *framework.ScenarioResult {
	return &framework.ScenarioResult{
		Name:      name,
		Framework: fw,
		Suite:     suite,
		Status:    "passed",
		StartedAt: time.Now(),
		Phases:    pt.Phases(),
		LiveURL:   liveURL,
		ProjectID: b.projectID,
	}
}

// Failed is a convenience builder for a failed result.
func (b *Base) Failed(pt *framework.PhaseTracker, name, suite, fw, reason string) *framework.ScenarioResult {
	return &framework.ScenarioResult{
		Name:       name,
		Framework:  fw,
		Suite:      suite,
		Status:     "failed",
		StartedAt:  time.Now(),
		Phases:     pt.Phases(),
		FailReason: reason,
		ProjectID:  b.projectID,
	}
}
