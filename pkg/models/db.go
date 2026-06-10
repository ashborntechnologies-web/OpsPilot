package models

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
}

// UserOwnsProject reports whether the given project belongs to the given user.
// Used as the tenant-isolation guard on every /projects/:id/... handler so a user
// cannot read or mutate another tenant's project by guessing its UUID.
func (db *DB) UserOwnsProject(ctx context.Context, userID, projectID uuid.UUID) (bool, error) {
	var exists bool
	err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1 AND user_id = $2)`,
		projectID, userID,
	).Scan(&exists)
	return exists, err
}

// UserOwnsAccount reports whether the given AWS account belongs to the given user.
func (db *DB) UserOwnsAccount(ctx context.Context, userID, accountID uuid.UUID) (bool, error) {
	var exists bool
	err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM aws_accounts WHERE id = $1 AND user_id = $2)`,
		accountID, userID,
	).Scan(&exists)
	return exists, err
}

func NewDB(databaseURL string) (*DB, error) {
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		return nil, fmt.Errorf("unable to create connection pool: %w", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("unable to ping database: %w", err)
	}

	return &DB{Pool: pool}, nil
}

func (db *DB) Close() {
	db.Pool.Close()
}

func RunMigrations(db *DB) error {
	// Migrations run in order — add new ones at the bottom only
	migrations := []string{
		createUsersTable,
		createProjectsTable,
		createEnvironmentsTable,
		createDeploymentsTable,
		createIncidentsTable,
		createConversationsTable,
		addInfraColumnsToEnvironments,
		createAWSAccountsTable,
		migrateToAccountModel,
		addStartCommandToProjects,
		addBranchToProjects,
		createPlatformStacksTable,
		addPlatformStackToEnvironments,
		createOperationalEventsTable,
		addExternalIDToAWSAccounts,
		addAccountScopeToOperationalEvents,
		relaxOperationalEventsAccountFK,
		createEnvVarsTable,
		createWebhooksTable,
		addPreviewColumnsToEnvironments,
		addWebhookColumnsToProjects,
	}

	for _, m := range migrations {
		if _, err := db.Pool.Exec(context.Background(), m); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	return nil
}

const createUsersTable = `
CREATE TABLE IF NOT EXISTS users (
	id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	clerk_id    TEXT UNIQUE NOT NULL,
	email       TEXT UNIQUE NOT NULL,
	github_token TEXT,       -- encrypted
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

const createProjectsTable = `
CREATE TABLE IF NOT EXISTS projects (
	id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	repo_url    TEXT NOT NULL,
	repo_owner  TEXT NOT NULL,
	repo_name   TEXT NOT NULL,
	framework   TEXT NOT NULL, -- fastapi | flask | nodejs | nextjs
	aws_region  TEXT NOT NULL DEFAULT 'us-east-1',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

const createEnvironmentsTable = `
CREATE TABLE IF NOT EXISTS environments (
	id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id              UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name                    TEXT NOT NULL CHECK (name IN ('staging', 'production')),
	aws_region              TEXT NOT NULL DEFAULT 'us-east-1',
	aws_account_id          TEXT,
	iam_role_arn            TEXT,         -- assumed role ARN
	cloudformation_stack_id TEXT,
	stack_status            TEXT NOT NULL DEFAULT 'pending', -- pending | provisioning | ready | failed
	alb_dns                 TEXT,         -- load balancer DNS once provisioned
	created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE(project_id, name)
);`

const createDeploymentsTable = `
CREATE TABLE IF NOT EXISTS deployments (
	id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	environment_id  UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
	commit_sha      TEXT NOT NULL,
	commit_message  TEXT,
	image_uri       TEXT,         -- ECR image URI
	status          TEXT NOT NULL DEFAULT 'pending', -- pending | building | deploying | live | failed | rolled_back
	failure_reason  TEXT,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

const createIncidentsTable = `
CREATE TABLE IF NOT EXISTS incidents (
	id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	deployment_id   UUID REFERENCES deployments(id),
	environment_id  UUID REFERENCES environments(id),
	trigger         TEXT NOT NULL, -- deploy_failure | runtime_anomaly | user_request
	root_cause      TEXT,
	resolution      TEXT,
	raw_logs        TEXT,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

// addInfraColumnsToEnvironments is idempotent — safe to run on existing DBs.
const addInfraColumnsToEnvironments = `
ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS ecr_repo_uri            TEXT,
    ADD COLUMN IF NOT EXISTS ecs_cluster_name        TEXT,
    ADD COLUMN IF NOT EXISTS ecs_service_name        TEXT,
    ADD COLUMN IF NOT EXISTS codebuild_project_name  TEXT,
    ADD COLUMN IF NOT EXISTS task_execution_role_arn TEXT,
    ADD COLUMN IF NOT EXISTS log_group_name          TEXT,
    ADD COLUMN IF NOT EXISTS alb_target_group_arn    TEXT,
    ADD COLUMN IF NOT EXISTS ecs_security_group_id   TEXT,
    ADD COLUMN IF NOT EXISTS vpc_subnets             TEXT;`

const createConversationsTable = `
CREATE TABLE IF NOT EXISTS conversations (
	id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role        TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	message     TEXT NOT NULL,
	intent      TEXT,   -- classified intent: deploy | rollback | logs | scale | diagnose | unknown
	metadata    JSONB,  -- extra context (deployment_id referenced, etc.)
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

const createAWSAccountsTable = `
CREATE TABLE IF NOT EXISTS aws_accounts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label          TEXT NOT NULL,
    aws_account_id TEXT NOT NULL,
    iam_role_arn   TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

const migrateToAccountModel = `
ALTER TABLE projects     ADD COLUMN IF NOT EXISTS account_id UUID REFERENCES aws_accounts(id);
ALTER TABLE environments ADD COLUMN IF NOT EXISTS account_id UUID REFERENCES aws_accounts(id);
ALTER TABLE environments DROP COLUMN IF EXISTS aws_account_id;
ALTER TABLE environments DROP COLUMN IF EXISTS iam_role_arn;`

const addStartCommandToProjects = `
ALTER TABLE projects ADD COLUMN IF NOT EXISTS start_command TEXT;`

const addBranchToProjects = `
ALTER TABLE projects ADD COLUMN IF NOT EXISTS branch TEXT;`

// createPlatformStacksTable stores shared infrastructure (VPC, ECS cluster, ALB) provisioned
// once per AWS account × region and reused across all project environments.
const createPlatformStacksTable = `
CREATE TABLE IF NOT EXISTS platform_stacks (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id          UUID NOT NULL REFERENCES aws_accounts(id) ON DELETE CASCADE,
    aws_region          TEXT NOT NULL,
    stack_id            TEXT,
    stack_status        TEXT NOT NULL DEFAULT 'pending',
    ecs_cluster_name    TEXT,
    alb_arn             TEXT,
    alb_dns             TEXT,
    alb_listener_arn    TEXT,
    alb_security_group_id TEXT,
    ecs_security_group_id TEXT,
    subnet_ids          TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(account_id, aws_region)
);`

const addPlatformStackToEnvironments = `
ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS platform_stack_id UUID REFERENCES platform_stacks(id),
    ADD COLUMN IF NOT EXISTS alb_listener_rule_arn TEXT;`

// addExternalIDToAWSAccounts adds the per-tenant STS external ID used in the role trust
// policy. Existing accounts default to the legacy shared value 'convdeploy' so their
// already-deployed bootstrap roles keep working; new accounts get a derived per-user value.
const addExternalIDToAWSAccounts = `
ALTER TABLE aws_accounts ADD COLUMN IF NOT EXISTS external_id TEXT NOT NULL DEFAULT 'convdeploy';`

// addAccountScopeToOperationalEvents lets the event system record account-scoped actions
// (e.g. external_id.generated) that occur before any project link exists. project_id is
// made nullable and an optional account_id is added. Existing project-scoped events are
// unaffected (they continue to set project_id).
const addAccountScopeToOperationalEvents = `
ALTER TABLE operational_events ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE operational_events ADD COLUMN IF NOT EXISTS account_id UUID REFERENCES aws_accounts(id);`

// relaxOperationalEventsAccountFK makes the account_id reference ON DELETE SET NULL.
// Without this, the external_id.generated audit row written at connection time pins every
// AWS account in place and blocks its deletion (FK RESTRICT). SET NULL preserves the audit
// record while letting the account be disconnected.
// createEnvVarsTable stores per-environment key/value pairs injected into the ECS task
// definition at deploy time. is_secret=true rows are redacted in API responses so secret
// values never appear in JSON (they are stored plaintext in Postgres, which is encrypted
// at rest; a SSM migration can be added later if per-secret audit is needed).
const createEnvVarsTable = `
CREATE TABLE IF NOT EXISTS env_vars (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    environment_id UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    key            TEXT NOT NULL,
    value          TEXT NOT NULL,
    is_secret      BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(environment_id, key)
);
CREATE INDEX IF NOT EXISTS idx_env_vars_environment ON env_vars(environment_id);`

const relaxOperationalEventsAccountFK = `
ALTER TABLE operational_events DROP CONSTRAINT IF EXISTS operational_events_account_id_fkey;
ALTER TABLE operational_events
    ADD CONSTRAINT operational_events_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES aws_accounts(id) ON DELETE SET NULL;`

// createOperationalEventsTable stores structured state-transition events for every
// deploy and provision operation. AI reasons over these events rather than raw log text.
const createOperationalEventsTable = `
CREATE TABLE IF NOT EXISTS operational_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    environment_id UUID REFERENCES environments(id),
    deployment_id  UUID REFERENCES deployments(id),
    event_type     TEXT NOT NULL,
    severity       TEXT NOT NULL DEFAULT 'info',
    source         TEXT NOT NULL DEFAULT 'deployer',
    actor_type     TEXT NOT NULL DEFAULT 'system',
    payload        JSONB,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_op_events_deployment ON operational_events(deployment_id);
CREATE INDEX IF NOT EXISTS idx_op_events_project    ON operational_events(project_id, occurred_at DESC);`

// addPreviewColumnsToEnvironments extends environments to support ephemeral PR preview
// environments. The CHECK and UNIQUE constraints from the original CREATE TABLE are relaxed
// so preview envs can have names like 'pr-42' and multiple previews can coexist per project.
const addPreviewColumnsToEnvironments = `
ALTER TABLE environments DROP CONSTRAINT IF EXISTS environments_name_check;
ALTER TABLE environments DROP CONSTRAINT IF EXISTS environments_project_id_name_key;
ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS is_preview           BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS pr_number            INTEGER,
    ADD COLUMN IF NOT EXISTS pr_branch            TEXT,
    ADD COLUMN IF NOT EXISTS pr_head_sha          TEXT,
    ADD COLUMN IF NOT EXISTS github_pr_comment_id BIGINT;
CREATE UNIQUE INDEX IF NOT EXISTS environments_non_preview_name_uniq
    ON environments(project_id, name) WHERE is_preview = false;
CREATE UNIQUE INDEX IF NOT EXISTS environments_preview_pr_uniq
    ON environments(project_id, pr_number) WHERE is_preview = true;`

// addWebhookColumnsToProjects stores the GitHub repo webhook installed when PR previews
// are enabled, so events can be verified (HMAC) and the hook can be removed on deletion.
const addWebhookColumnsToProjects = `
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS github_webhook_id     BIGINT,
    ADD COLUMN IF NOT EXISTS github_webhook_secret TEXT NOT NULL DEFAULT '';`

const createWebhooksTable = `
CREATE TABLE IF NOT EXISTS webhooks (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    url        TEXT NOT NULL,
    secret     TEXT NOT NULL DEFAULT '',
    events     TEXT[] NOT NULL DEFAULT '{}',
    active     BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_webhooks_project ON webhooks(project_id);`
