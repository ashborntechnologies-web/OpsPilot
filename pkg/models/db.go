package models

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
}

// ErrNoMembership signals that a user is not a member of the relevant organization
// (or the resource doesn't exist). Callers should map it to 404, not 403, so org
// and resource existence is not leaked across tenants.
var ErrNoMembership = errors.New("no organization membership")

// UserOrgRole returns the user's role in the org, or ErrNoMembership if they are
// not a member. This is the tenant-isolation primitive for /orgs/:orgId routes.
func (db *DB) UserOrgRole(ctx context.Context, userID, orgID uuid.UUID) (string, error) {
	var role string
	err := db.Pool.QueryRow(ctx,
		`SELECT role FROM organization_members WHERE org_id = $1 AND user_id = $2`,
		orgID, userID,
	).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoMembership
	}
	return role, err
}

// ProjectOrgRole resolves the org that owns a project and the requesting user's
// role in that org. Returns ErrNoMembership when the project doesn't exist, has no
// org, or the user isn't a member — the tenant-isolation guard for /projects/:id.
func (db *DB) ProjectOrgRole(ctx context.Context, userID, projectID uuid.UUID) (orgID uuid.UUID, role string, err error) {
	err = db.Pool.QueryRow(ctx,
		`SELECT p.org_id, m.role
		   FROM projects p
		   JOIN organization_members m ON m.org_id = p.org_id AND m.user_id = $2
		  WHERE p.id = $1`,
		projectID, userID,
	).Scan(&orgID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.UUID{}, "", ErrNoMembership
	}
	return orgID, role, err
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
		addHTTPSSupport,
		createDiagnosisFeedbackTable,
		createAlertsTable,
		createAlertPreferencesTable,
		createProjectMemoryTable,
		addNotificationAndPlanToUsers,
		addBuildIDToDeployments,
		createOrganizationsTable,
		createOrganizationMembersTable,
		createOrganizationInvitesTable,
		addOrgIDColumns,
		backfillPersonalOrgs,
		createDiscoveredResourcesTable,
		addLastScannedAtToAWSAccounts,
		extendIncidentsForWarRoom,
		createIncidentTimelineTable,
		createIncidentActionsTable,
		createSlackIntegrationsTable,
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

// createAlertsTable holds user-facing alerts derived from operational events by
// the alert engine — deduplicated, AI-summarized, resolvable.
const createAlertsTable = `
CREATE TABLE IF NOT EXISTS alerts (
	id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id       UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	environment_id   UUID REFERENCES environments(id) ON DELETE CASCADE,
	alert_type       TEXT NOT NULL,
	severity         TEXT NOT NULL DEFAULT 'warn',
	title            TEXT NOT NULL,
	summary          TEXT NOT NULL,
	status           TEXT NOT NULL DEFAULT 'open',
	triggered_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	resolved_at      TIMESTAMPTZ,
	snoozed_until    TIMESTAMPTZ,
	source_event_ids UUID[] NOT NULL DEFAULT '{}',
	created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_alerts_project ON alerts(project_id, status, triggered_at DESC);`

// createAlertPreferencesTable stores per-environment alert snoozes so the alert
// engine can suppress alert types the user has muted.
const createAlertPreferencesTable = `
CREATE TABLE IF NOT EXISTS alert_preferences (
	id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id     UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	environment_id UUID REFERENCES environments(id) ON DELETE CASCADE,
	alert_type     TEXT NOT NULL,
	snoozed_until  TIMESTAMPTZ,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE(project_id, environment_id, alert_type)
);`

// createProjectMemoryTable is OpsPilot's long-term memory: facts learned about a
// project (recurring failures, confirmed fixes, deploy patterns) that are
// injected into future diagnosis prompts.
const createProjectMemoryTable = `
CREATE TABLE IF NOT EXISTS project_memory (
	id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id         UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	memory_type        TEXT NOT NULL,
	content            TEXT NOT NULL,
	confidence         FLOAT NOT NULL DEFAULT 1.0,
	source             TEXT NOT NULL,
	created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_referenced_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	reference_count    INT NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_project_memory_project
	ON project_memory(project_id, memory_type, last_referenced_at DESC);`

// addNotificationAndPlanToUsers: per-user notification toggles plus plan and
// AI-action metering for usage limits.
const addNotificationAndPlanToUsers = `
ALTER TABLE users ADD COLUMN IF NOT EXISTS notifications_enabled    BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE users ADD COLUMN IF NOT EXISTS notify_deploy_failed     BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE users ADD COLUMN IF NOT EXISTS notify_deploy_succeeded  BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN IF NOT EXISTS notify_alert_fired       BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan                     TEXT NOT NULL DEFAULT 'free';
ALTER TABLE users ADD COLUMN IF NOT EXISTS ai_actions_this_month    INT NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS ai_actions_reset_at      TIMESTAMPTZ NOT NULL DEFAULT NOW();`

// addBuildIDToDeployments stores the CodeBuild build ID so an in-flight build
// can be cancelled and its logs streamed.
const addBuildIDToDeployments = `
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS build_id TEXT;`

// createDiagnosisFeedbackTable records user ratings of AI diagnoses. helpful+fixed
// rows are the gold-standard dataset for improving the diagnosis model.
const createDiagnosisFeedbackTable = `
CREATE TABLE IF NOT EXISTS diagnosis_feedback (
	id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	incident_id UUID NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
	project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	user_id     UUID NOT NULL REFERENCES users(id),
	rating      TEXT NOT NULL CHECK (rating IN ('helpful', 'not_helpful', 'partially_helpful')),
	fixed_issue BOOLEAN NOT NULL DEFAULT false,
	notes       TEXT NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (incident_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_diag_feedback_project ON diagnosis_feedback(project_id);`

// addHTTPSSupport: an optional per-account ACM certificate enables an HTTPS listener
// on the shared ALB; platform stacks record whether they were provisioned with one
// (alb_listener_arn then holds the 443 listener and app URLs use https).
const addHTTPSSupport = `
ALTER TABLE aws_accounts    ADD COLUMN IF NOT EXISTS certificate_arn TEXT;
ALTER TABLE platform_stacks ADD COLUMN IF NOT EXISTS https_enabled BOOLEAN NOT NULL DEFAULT false;`

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

// ─── Organizations & RBAC ────────────────────────────────────────────────────

// createOrganizationsTable: a team workspace. slug is URL-safe and unique.
const createOrganizationsTable = `
CREATE TABLE IF NOT EXISTS organizations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    slug       TEXT UNIQUE NOT NULL,
    created_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

// createOrganizationMembersTable: a user's role in an org. One row per (org,user).
const createOrganizationMembersTable = `
CREATE TABLE IF NOT EXISTS organization_members (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'engineer', 'viewer')),
    invited_by UUID REFERENCES users(id),
    joined_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_org_members_user ON organization_members(user_id);
CREATE INDEX IF NOT EXISTS idx_org_members_org  ON organization_members(org_id);`

// createOrganizationInvitesTable: pending invitations redeemable by token.
const createOrganizationInvitesTable = `
CREATE TABLE IF NOT EXISTS organization_invites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email       TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'engineer', 'viewer')),
    token       UUID NOT NULL DEFAULT gen_random_uuid(),
    invited_by  UUID NOT NULL REFERENCES users(id),
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '7 days'),
    accepted_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_org_invites_token ON organization_invites(token);
CREATE INDEX IF NOT EXISTS idx_org_invites_org ON organization_invites(org_id);`

// addOrgIDColumns: tenant ownership moves from user_id to org_id. Columns are
// nullable here and backfilled by backfillPersonalOrgs below; the application
// always sets org_id on new rows.
const addOrgIDColumns = `
ALTER TABLE projects     ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
ALTER TABLE aws_accounts ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
ALTER TABLE alerts       ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
ALTER TABLE incidents    ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_projects_org     ON projects(org_id);
CREATE INDEX IF NOT EXISTS idx_aws_accounts_org ON aws_accounts(org_id);
CREATE INDEX IF NOT EXISTS idx_alerts_org       ON alerts(org_id);`

// createDiscoveredResourcesTable stores AWS resources found by the discovery scanner
// in connected accounts. is_managed marks OpsPilot-created resources (ManagedBy tag);
// project_id is NULL until a user assigns the resource to a project. The unique key
// (org_id, resource_type, resource_id) makes scans idempotent (upsert on re-scan).
const createDiscoveredResourcesTable = `
CREATE TABLE IF NOT EXISTS discovered_resources (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    aws_account_id UUID NOT NULL REFERENCES aws_accounts(id) ON DELETE CASCADE,
    resource_type  TEXT NOT NULL,
    resource_id    TEXT NOT NULL,
    resource_name  TEXT NOT NULL DEFAULT '',
    region         TEXT NOT NULL DEFAULT '',
    metadata       JSONB NOT NULL DEFAULT '{}',
    tags           JSONB NOT NULL DEFAULT '{}',
    project_id     UUID REFERENCES projects(id) ON DELETE SET NULL,
    is_managed     BOOLEAN NOT NULL DEFAULT false,
    first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, resource_type, resource_id)
);
CREATE INDEX IF NOT EXISTS idx_discovered_org     ON discovered_resources(org_id, resource_type);
CREATE INDEX IF NOT EXISTS idx_discovered_account ON discovered_resources(aws_account_id);
CREATE INDEX IF NOT EXISTS idx_discovered_project ON discovered_resources(project_id);`

// addLastScannedAtToAWSAccounts records when the discovery scanner last completed for
// an account (surfaced in the UI).
const addLastScannedAtToAWSAccounts = `
ALTER TABLE aws_accounts ADD COLUMN IF NOT EXISTS last_scanned_at TIMESTAMPTZ;`

// extendIncidentsForWarRoom turns incidents into first-class, lifecycle-tracked objects
// for the incident war room: a title, status (open→investigating→resolved), severity,
// who acknowledged/resolved and when, and an AI-generated postmortem. org_id was already
// added by addOrgIDColumns; included here idempotently. The status CHECK is added as a
// named constraint so it is skipped if already present.
const extendIncidentsForWarRoom = `
ALTER TABLE incidents
    ADD COLUMN IF NOT EXISTS title           TEXT,
    ADD COLUMN IF NOT EXISTS status          TEXT NOT NULL DEFAULT 'open',
    ADD COLUMN IF NOT EXISTS severity        TEXT NOT NULL DEFAULT 'warn',
    ADD COLUMN IF NOT EXISTS acknowledged_by UUID REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS acknowledged_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resolved_by     UUID REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS resolved_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS postmortem      TEXT,
    ADD COLUMN IF NOT EXISTS org_id          UUID REFERENCES organizations(id) ON DELETE CASCADE;
DO $$ BEGIN
    ALTER TABLE incidents ADD CONSTRAINT incidents_status_check
        CHECK (status IN ('open', 'investigating', 'resolved'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;
CREATE INDEX IF NOT EXISTS idx_incidents_org    ON incidents(org_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_incidents_status ON incidents(project_id, status, created_at DESC);`

// createIncidentTimelineTable stores the war-room feed: AI and human entries posted as
// an incident is investigated. author_id is NULL for AI entries.
const createIncidentTimelineTable = `
CREATE TABLE IF NOT EXISTS incident_timeline (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id UUID NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    author_type TEXT NOT NULL CHECK (author_type IN ('ai', 'human')),
    author_id   UUID REFERENCES users(id),
    content     TEXT NOT NULL,
    entry_type  TEXT NOT NULL DEFAULT 'update', -- diagnosis | update | action_taken | resolution
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_incident_timeline ON incident_timeline(incident_id, created_at);`

// createSlackIntegrationsTable stores one Slack workspace connection per org. The bot
// token is encrypted at rest with ENCRYPTION_KEY (pkg/crypto). Channel IDs/names for
// alerts, deploy notifications, and the daily summary are configurable.
const createSlackIntegrationsTable = `
CREATE TABLE IF NOT EXISTS slack_integrations (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id               UUID NOT NULL UNIQUE REFERENCES organizations(id) ON DELETE CASCADE,
    team_id              TEXT NOT NULL DEFAULT '', -- Slack workspace ID (maps slash commands → org)
    workspace_name       TEXT NOT NULL DEFAULT '',
    bot_token            TEXT NOT NULL, -- encrypted
    alert_channel_id     TEXT,
    alert_channel_name   TEXT,
    deploy_channel_id    TEXT,
    deploy_channel_name  TEXT,
    summary_channel_id   TEXT,
    summary_channel_name TEXT,
    installed_by         UUID REFERENCES users(id),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_slack_team ON slack_integrations(team_id);`

// createIncidentActionsTable stores remediation actions proposed during an incident
// (by the AI or a human) and their approval lifecycle.
const createIncidentActionsTable = `
CREATE TABLE IF NOT EXISTS incident_actions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id UUID NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    proposed_by TEXT NOT NULL CHECK (proposed_by IN ('ai', 'human')),
    action_type TEXT NOT NULL,
    parameters  JSONB NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'executed', 'rejected')),
    approved_by UUID REFERENCES users(id),
    executed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_incident_actions ON incident_actions(incident_id, created_at);`

// backfillPersonalOrgs migrates every existing user into a personal organization
// (admin membership) and assigns all of their existing data to it. Idempotent:
// users who already have a membership are skipped; only NULL org_id rows are
// backfilled. New users (post-migration) get their personal org created in the
// auth-middleware user-upsert path.
const backfillPersonalOrgs = `
DO $$
DECLARE
    u RECORD;
    new_org UUID;
    base TEXT;
BEGIN
    FOR u IN SELECT id, email FROM users LOOP
        IF EXISTS (SELECT 1 FROM organization_members WHERE user_id = u.id) THEN
            CONTINUE;
        END IF;
        base := NULLIF(regexp_replace(lower(split_part(u.email, '@', 1)), '[^a-z0-9]+', '-', 'g'), '');
        INSERT INTO organizations (name, slug, created_by)
        VALUES (
            COALESCE(initcap(base), 'Personal') || ' (personal)',
            'u-' || replace(u.id::text, '-', '')
        , u.id)
        RETURNING id INTO new_org;

        INSERT INTO organization_members (org_id, user_id, role, invited_by)
        VALUES (new_org, u.id, 'admin', u.id);

        UPDATE projects     SET org_id = new_org WHERE user_id = u.id AND org_id IS NULL;
        UPDATE aws_accounts SET org_id = new_org WHERE user_id = u.id AND org_id IS NULL;
    END LOOP;

    -- alerts / incidents inherit their project's org.
    UPDATE alerts a    SET org_id = p.org_id FROM projects p WHERE a.project_id = p.id AND a.org_id IS NULL;
    UPDATE incidents i SET org_id = p.org_id FROM projects p WHERE i.project_id = p.id AND i.org_id IS NULL;
END $$;`
