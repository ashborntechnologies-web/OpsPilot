package models

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
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
