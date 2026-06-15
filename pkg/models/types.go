package models

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID          uuid.UUID `json:"id" db:"id"`
	ClerkID     string    `json:"clerk_id" db:"clerk_id"`
	Email       string    `json:"email" db:"email"`
	GithubToken *string   `json:"-" db:"github_token"` // never expose in JSON
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

type AWSAccount struct {
	ID           uuid.UUID  `json:"id" db:"id"`
	UserID       uuid.UUID  `json:"user_id" db:"user_id"`
	OrgID        *uuid.UUID `json:"org_id" db:"org_id"`
	Label        string     `json:"label" db:"label"`
	AWSAccountID string     `json:"aws_account_id" db:"aws_account_id"`
	IAMRoleARN   string     `json:"iam_role_arn" db:"iam_role_arn"`
	ExternalID   string     `json:"-" db:"external_id"` // STS external ID; per-tenant, not exposed in JSON
	// CertificateARN is an optional ACM cert that enables HTTPS on the shared ALB
	// for platform stacks provisioned in this account.
	CertificateARN *string `json:"certificate_arn,omitempty" db:"certificate_arn"`
	// LastScannedAt is when the discovery scanner last ran for this account.
	LastScannedAt *time.Time `json:"last_scanned_at" db:"last_scanned_at"`
	CreatedAt     time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at" db:"updated_at"`
}

type Project struct {
	ID                  uuid.UUID  `json:"id" db:"id"`
	UserID              uuid.UUID  `json:"user_id" db:"user_id"`
	OrgID               *uuid.UUID `json:"org_id" db:"org_id"`
	Name                string     `json:"name" db:"name"`
	RepoURL             string     `json:"repo_url" db:"repo_url"`
	RepoOwner           string     `json:"repo_owner" db:"repo_owner"`
	RepoName            string     `json:"repo_name" db:"repo_name"`
	Framework           string     `json:"framework" db:"framework"`
	Branch              *string    `json:"branch" db:"branch"`
	StartCommand        *string    `json:"start_command" db:"start_command"`
	AccountID           *uuid.UUID `json:"account_id" db:"account_id"`
	GithubWebhookID     *int64     `json:"github_webhook_id,omitempty" db:"github_webhook_id"`
	GithubWebhookSecret string     `json:"-" db:"github_webhook_secret"` // never exposed
	CreatedAt           time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at" db:"updated_at"`
	// PreviewsEnabled is derived (true when GithubWebhookID is non-nil).
	PreviewsEnabled bool `json:"previews_enabled" db:"-"`
}

// PlatformStack represents the shared infrastructure (VPC, ECS cluster, ALB) provisioned
// once per AWS account × region. Multiple project environments share a single platform stack.
type PlatformStack struct {
	ID             uuid.UUID `json:"id" db:"id"`
	AccountID      uuid.UUID `json:"account_id" db:"account_id"`
	AWSRegion      string    `json:"aws_region" db:"aws_region"`
	StackID        *string   `json:"cloudformation_stack_id" db:"stack_id"`
	StackStatus    string    `json:"stack_status" db:"stack_status"`
	ECSClusterName *string   `json:"ecs_cluster_name" db:"ecs_cluster_name"`
	ALBArn         *string   `json:"alb_arn" db:"alb_arn"`
	ALBDNS         *string   `json:"alb_dns" db:"alb_dns"`
	ALBListenerArn *string   `json:"alb_listener_arn" db:"alb_listener_arn"`
	// HTTPSEnabled is true when the stack was provisioned with an ACM certificate —
	// ALBListenerArn then refers to the 443 listener and app URLs use https://.
	HTTPSEnabled       bool      `json:"https_enabled" db:"https_enabled"`
	ALBSecurityGroupID *string   `json:"alb_security_group_id" db:"alb_security_group_id"`
	ECSSecurityGroupID *string   `json:"ecs_security_group_id" db:"ecs_security_group_id"`
	SubnetIDs          *string   `json:"subnet_ids" db:"subnet_ids"` // comma-separated
	CreatedAt          time.Time `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time `json:"updated_at" db:"updated_at"`
}

type Environment struct {
	ID              uuid.UUID  `json:"id" db:"id"`
	ProjectID       uuid.UUID  `json:"project_id" db:"project_id"`
	Name            string     `json:"name" db:"name"` // staging | production
	AWSRegion       string     `json:"aws_region" db:"aws_region"`
	AccountID       *uuid.UUID `json:"account_id" db:"account_id"`
	PlatformStackID *uuid.UUID `json:"platform_stack_id" db:"platform_stack_id"`

	// CloudFormation stack tracking for project-level stack
	CloudFormationStackID *string `json:"cloudformation_stack_id" db:"cloudformation_stack_id"`
	StackStatus           string  `json:"stack_status" db:"stack_status"`

	// Project stack outputs (ECR, IAM, CodeBuild, log group)
	ECRRepoURI           *string `json:"ecr_repo_uri" db:"ecr_repo_uri"`
	ECSServiceName       *string `json:"ecs_service_name" db:"ecs_service_name"`
	CodeBuildProjectName *string `json:"codebuild_project_name" db:"codebuild_project_name"`
	TaskExecutionRoleARN *string `json:"task_execution_role_arn" db:"task_execution_role_arn"`
	LogGroupName         *string `json:"log_group_name" db:"log_group_name"`

	// Deploy-time resources (created by SDK at each deploy, not CF)
	ALBTargetGroupARN  *string `json:"alb_target_group_arn" db:"alb_target_group_arn"`
	ALBListenerRuleARN *string `json:"alb_listener_rule_arn" db:"alb_listener_rule_arn"`
	ALBDNS             *string `json:"alb_dns" db:"alb_dns"` // platform ALB DNS (convenience)

	// Legacy single-stack fields — populated for pre-platform-stack environments only.
	// New environments source these from the platform_stacks table via PlatformStackID.
	ECSClusterName     *string `json:"ecs_cluster_name" db:"ecs_cluster_name"`
	ECSSecurityGroupID *string `json:"ecs_security_group_id" db:"ecs_security_group_id"`
	VPCSubnets         *string `json:"vpc_subnets" db:"vpc_subnets"`

	// PR preview fields — only set when IsPreview = true.
	IsPreview         bool    `json:"is_preview" db:"is_preview"`
	PRNumber          *int    `json:"pr_number,omitempty" db:"pr_number"`
	PRBranch          *string `json:"pr_branch,omitempty" db:"pr_branch"`
	PRHeadSHA         *string `json:"pr_head_sha,omitempty" db:"pr_head_sha"`
	GithubPRCommentID *int64  `json:"github_pr_comment_id,omitempty" db:"github_pr_comment_id"`

	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

type Deployment struct {
	ID            uuid.UUID `json:"id" db:"id"`
	ProjectID     uuid.UUID `json:"project_id" db:"project_id"`
	EnvironmentID uuid.UUID `json:"environment_id" db:"environment_id"`
	CommitSHA     string    `json:"commit_sha" db:"commit_sha"`
	CommitMessage *string   `json:"commit_message" db:"commit_message"`
	ImageURI      *string   `json:"image_uri" db:"image_uri"`
	Status        string    `json:"status" db:"status"` // pending | building | deploying | live | failed | rolled_back
	FailureReason *string   `json:"failure_reason" db:"failure_reason"`
	CreatedAt     time.Time `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time `json:"updated_at" db:"updated_at"`
}

type Incident struct {
	ID            uuid.UUID  `json:"id" db:"id"`
	ProjectID     uuid.UUID  `json:"project_id" db:"project_id"`
	OrgID         *uuid.UUID `json:"org_id" db:"org_id"`
	DeploymentID  *uuid.UUID `json:"deployment_id" db:"deployment_id"`
	EnvironmentID *uuid.UUID `json:"environment_id" db:"environment_id"`
	Trigger       string     `json:"trigger" db:"trigger"` // deploy_failure | runtime_anomaly | user_request
	RootCause     *string    `json:"root_cause" db:"root_cause"`
	Resolution    *string    `json:"resolution" db:"resolution"`
	RawLogs       *string    `json:"-" db:"raw_logs"`

	// War-room lifecycle fields.
	Title          *string    `json:"title" db:"title"`
	Status         string     `json:"status" db:"status"` // open | investigating | resolved
	Severity       string     `json:"severity" db:"severity"`
	AcknowledgedBy *uuid.UUID `json:"acknowledged_by" db:"acknowledged_by"`
	AcknowledgedAt *time.Time `json:"acknowledged_at" db:"acknowledged_at"`
	ResolvedBy     *uuid.UUID `json:"resolved_by" db:"resolved_by"`
	ResolvedAt     *time.Time `json:"resolved_at" db:"resolved_at"`
	Postmortem     *string    `json:"postmortem" db:"postmortem"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`

	// Joined/derived fields (not columns) — populated by list/get queries.
	EnvironmentName    string `json:"environment_name,omitempty" db:"-"`
	ProjectName        string `json:"project_name,omitempty" db:"-"`
	AcknowledgedByName string `json:"acknowledged_by_name,omitempty" db:"-"`
}

// Incident status + severity constants.
const (
	IncidentStatusOpen          = "open"
	IncidentStatusInvestigating = "investigating"
	IncidentStatusResolved      = "resolved"
)

// Incident trigger constants.
const (
	IncidentTriggerDeployFailure  = "deploy_failure"
	IncidentTriggerRuntimeAnomaly = "runtime_anomaly"
	IncidentTriggerUserRequest    = "user_request"
)

// Incident author + entry-type + action-status constants.
const (
	IncidentAuthorAI    = "ai"
	IncidentAuthorHuman = "human"

	IncidentEntryDiagnosis   = "diagnosis"
	IncidentEntryUpdate      = "update"
	IncidentEntryActionTaken = "action_taken"
	IncidentEntryResolution  = "resolution"

	IncidentActionPending  = "pending"
	IncidentActionApproved = "approved"
	IncidentActionExecuted = "executed"
	IncidentActionRejected = "rejected"
)

// IncidentTimelineEntry is one post in the war-room feed — an AI diagnosis/update or a
// human comment. author_id is nil for AI entries.
type IncidentTimelineEntry struct {
	ID         uuid.UUID      `json:"id" db:"id"`
	IncidentID uuid.UUID      `json:"incident_id" db:"incident_id"`
	AuthorType string         `json:"author_type" db:"author_type"`
	AuthorID   *uuid.UUID     `json:"author_id" db:"author_id"`
	Content    string         `json:"content" db:"content"`
	EntryType  string         `json:"entry_type" db:"entry_type"`
	Metadata   map[string]any `json:"metadata" db:"metadata"`
	CreatedAt  time.Time      `json:"created_at" db:"created_at"`
	// AuthorName is the human author's email — populated by the timeline query.
	AuthorName string `json:"author_name,omitempty" db:"-"`
}

// IncidentAction is a remediation step proposed during an incident, with an approval
// lifecycle (pending → approved/executed | rejected).
type IncidentAction struct {
	ID         uuid.UUID      `json:"id" db:"id"`
	IncidentID uuid.UUID      `json:"incident_id" db:"incident_id"`
	ProposedBy string         `json:"proposed_by" db:"proposed_by"`
	ActionType string         `json:"action_type" db:"action_type"`
	Parameters map[string]any `json:"parameters" db:"parameters"`
	Status     string         `json:"status" db:"status"`
	ApprovedBy *uuid.UUID     `json:"approved_by" db:"approved_by"`
	ExecutedAt *time.Time     `json:"executed_at" db:"executed_at"`
	CreatedAt  time.Time      `json:"created_at" db:"created_at"`
	// ApprovedByName is the approver's email — populated by the actions query.
	ApprovedByName string `json:"approved_by_name,omitempty" db:"-"`
}

type Conversation struct {
	ID        uuid.UUID `json:"id" db:"id"`
	ProjectID uuid.UUID `json:"project_id" db:"project_id"`
	UserID    uuid.UUID `json:"user_id" db:"user_id"`
	Role      string    `json:"role" db:"role"` // user | assistant
	Message   string    `json:"message" db:"message"`
	Intent    *string   `json:"intent" db:"intent"`
	Metadata  *string   `json:"metadata" db:"metadata"` // JSONB stored as string
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// Framework constants
const (
	// Python
	FrameworkFastAPI = "fastapi"
	FrameworkFlask   = "flask"
	FrameworkDjango  = "django"
	FrameworkPython  = "python"

	// Node.js / JavaScript
	FrameworkNodeJS    = "nodejs"
	FrameworkExpress   = "express"
	FrameworkNextJS    = "nextjs"
	FrameworkNestJS    = "nestjs"
	FrameworkRemix     = "remix"
	FrameworkNuxtJS    = "nuxtjs"
	FrameworkSvelteKit = "svelte"
	FrameworkAstro     = "astro"
	FrameworkReactSPA  = "react-spa"
	FrameworkVite      = "vite"

	// Go
	FrameworkGo = "go"

	// Ruby
	FrameworkRails = "rails"

	// Java
	FrameworkSpring = "spring"

	// Static
	FrameworkStatic = "static"
)

// Deployment status constants
const (
	DeployStatusPending    = "pending"
	DeployStatusBuilding   = "building"
	DeployStatusDeploying  = "deploying"
	DeployStatusLive       = "live"
	DeployStatusFailed     = "failed"
	DeployStatusRolledBack = "rolled_back"
)

// Stack status constants
const (
	StackStatusPending      = "pending"
	StackStatusProvisioning = "provisioning"
	StackStatusReady        = "ready"
	StackStatusFailed       = "failed"
)

// Intent constants (conversation engine)
const (
	IntentDeploy          = "deploy"
	IntentRedeploy        = "redeploy"
	IntentRollback        = "rollback"
	IntentScale           = "scale"
	IntentLogs            = "logs"
	IntentHealth          = "health"
	IntentDiagnose        = "diagnose"
	IntentCost            = "cost"
	IntentChangeResources = "change_resources"
	IntentConfirm         = "confirm"
	IntentUnknown         = "unknown"
)

// CostSummary holds a 30-day cost breakdown for a project's AWS account.
type CostSummary struct {
	TotalMonthlyCost float64            `json:"total_monthly_cost"`
	ByService        map[string]float64 `json:"by_service"`
	Currency         string             `json:"currency"`
	PeriodStart      string             `json:"period_start"`
	PeriodEnd        string             `json:"period_end"`
}

// MutationProposal holds a pending infra change waiting for user confirmation.
type MutationProposal struct {
	ProjectID     uuid.UUID `json:"project_id"`
	EnvName       string    `json:"env_name"`
	CPU           string    `json:"cpu"`
	Memory        string    `json:"memory"`
	CurrentCPU    string    `json:"current_cpu"`
	CurrentMemory string    `json:"current_memory"`
}

// EnvVar is a key/value environment variable scoped to a specific environment. It is
// injected into the ECS task definition at deploy time. Secret values (is_secret=true)
// are redacted in API responses but stored in full so the deployer can inject them.
type EnvVar struct {
	ID            uuid.UUID `json:"id"`
	EnvironmentID uuid.UUID `json:"environment_id"`
	Key           string    `json:"key"`
	Value         string    `json:"value,omitempty"` // omitted in list responses for secrets
	IsSecret      bool      `json:"is_secret"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Webhook struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	URL       string    `json:"url"`
	Secret    string    `json:"-"` // never returned in JSON; used only for HMAC signing
	Events    []string  `json:"events"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WebhookEvent constants
const (
	WebhookEventDeployStarted   = "deploy.started"
	WebhookEventDeploySucceeded = "deploy.succeeded"
	WebhookEventDeployFailed    = "deploy.failed"
)

// OperationalEvent is a structured record of a meaningful platform state transition.
// AI reasons over these events rather than raw log text.
type OperationalEvent struct {
	ID            uuid.UUID      `json:"id"`
	ProjectID     uuid.UUID      `json:"project_id"`
	EnvironmentID *uuid.UUID     `json:"environment_id"`
	DeploymentID  *uuid.UUID     `json:"deployment_id"`
	EventType     string         `json:"event_type"`
	Severity      string         `json:"severity"`
	Source        string         `json:"source"`
	ActorType     string         `json:"actor_type"`
	Payload       map[string]any `json:"payload"`
	OccurredAt    time.Time      `json:"occurred_at"`
}

// Operational event type constants
const (
	EventDeployStarted     = "deploy.started"
	EventBuildStarted      = "build.started"
	EventBuildCompleted    = "build.completed"
	EventBuildFailed       = "build.failed"
	EventECSRolloutStarted = "ecs.rollout.started"
	EventECSStable         = "ecs.stable"
	EventHealthcheckFailed = "healthcheck.failed"
	EventRollbackTriggered = "rollback.triggered"
	EventProvisionStarted  = "provision.started"
	EventProvisionReady    = "provision.ready"
	EventProvisionFailed   = "provision.failed"

	// Phase 2 — security
	EventExternalIDGenerated = "external_id.generated"
	EventSecretCreated       = "secret.created"
	EventSecretDeleted       = "secret.deleted"

	// Phase 2 — AI diagnosis
	EventDiagnosisStarted   = "diagnosis.started"
	EventDiagnosisCompleted = "diagnosis.completed"

	// Phase 2 — reliability watchdog
	EventDeploymentStuck      = "deployment.stuck"
	EventDeploymentAutoFailed = "deployment.auto_failed"
)

// Severity constants
const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Source constants
const (
	SourceDeployer  = "deployer"
	SourceECS       = "ecs"
	SourceALB       = "alb"
	SourceBuild     = "build"
	SourceScheduler = "scheduler"
	SourceAI        = "ai"
)

// Actor type constants
const (
	ActorSystem = "system"
	ActorUser   = "user"
	ActorAI     = "ai"
)

// Diagnosis feedback rating values
const (
	RatingHelpful          = "helpful"
	RatingNotHelpful       = "not_helpful"
	RatingPartiallyHelpful = "partially_helpful"
)

// DiagnosisFeedback is a user's rating of an AI-generated diagnosis. Rows rated
// helpful with fixed_issue=true form the verified-fix training dataset.
type DiagnosisFeedback struct {
	ID         uuid.UUID `json:"id" db:"id"`
	IncidentID uuid.UUID `json:"incident_id" db:"incident_id"`
	ProjectID  uuid.UUID `json:"project_id" db:"project_id"`
	UserID     uuid.UUID `json:"user_id" db:"user_id"`
	Rating     string    `json:"rating" db:"rating"` // helpful | not_helpful | partially_helpful
	FixedIssue bool      `json:"fixed_issue" db:"fixed_issue"`
	Notes      string    `json:"notes" db:"notes"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
}

// RatingScore converts a feedback row to a numeric quality signal:
// 1.0 for a helpful diagnosis that fixed the issue, 0.5 for partially helpful,
// 0.0 for not helpful (and for "helpful" claims that didn't actually fix it).
func (f *DiagnosisFeedback) RatingScore() float64 {
	switch f.Rating {
	case RatingHelpful:
		if f.FixedIssue {
			return 1.0
		}
		return 0.5
	case RatingPartiallyHelpful:
		return 0.5
	default:
		return 0.0
	}
}

// Runtime monitoring event types (emitted by the continuous health poller and
// log scanner between deploys).
const (
	EventRuntimeTasksDegraded    = "runtime.tasks_degraded"
	EventRuntimeServiceDown      = "runtime.service_down"
	EventRuntimeHighErrorRate    = "runtime.high_error_rate"
	EventRuntimeHighLatency      = "runtime.high_latency"
	EventRuntimeServiceRecovered = "runtime.service_recovered"
	EventRuntimeLogAnomaly       = "runtime.log_anomaly"
	EventAlertFired              = "alert.fired"
	EventAlertResolved           = "alert.resolved"
)

// Alert type constants
const (
	AlertTypeServiceDown   = "service_down"
	AlertTypeTasksDegraded = "tasks_degraded"
	AlertTypeHighErrorRate = "high_error_rate"
	AlertTypeHighLatency   = "high_latency"
	AlertTypeCrashLoop     = "crash_loop"
	AlertTypeLogAnomaly    = "log_anomaly"
	AlertTypeDeployStuck   = "deploy_stuck"
)

// Alert status constants
const (
	AlertStatusOpen     = "open"
	AlertStatusResolved = "resolved"
	AlertStatusSnoozed  = "snoozed"
)

// Alert is a deduplicated, user-facing notification derived from operational
// events by the alert engine. Alerts carry an AI-generated summary and live
// until resolved (automatically on recovery, or manually).
type Alert struct {
	ID             uuid.UUID   `json:"id" db:"id"`
	ProjectID      uuid.UUID   `json:"project_id" db:"project_id"`
	OrgID          *uuid.UUID  `json:"org_id" db:"org_id"`
	EnvironmentID  *uuid.UUID  `json:"environment_id" db:"environment_id"`
	AlertType      string      `json:"alert_type" db:"alert_type"`
	Severity       string      `json:"severity" db:"severity"`
	Title          string      `json:"title" db:"title"`
	Summary        string      `json:"summary" db:"summary"`
	Status         string      `json:"status" db:"status"`
	TriggeredAt    time.Time   `json:"triggered_at" db:"triggered_at"`
	ResolvedAt     *time.Time  `json:"resolved_at" db:"resolved_at"`
	SnoozedUntil   *time.Time  `json:"snoozed_until" db:"snoozed_until"`
	SourceEventIDs []uuid.UUID `json:"source_event_ids" db:"source_event_ids"`
	CreatedAt      time.Time   `json:"created_at" db:"created_at"`
}

// Project memory type constants
const (
	MemoryRecurringFailure = "recurring_failure"
	MemorySuccessfulFix    = "successful_fix"
	MemoryDeployPattern    = "deploy_pattern"
	MemoryAlertPreference  = "alert_preference"
	MemoryInfraPattern     = "infra_pattern"
)

// Memory source constants
const (
	MemorySourceDiagnosis       = "diagnosis"
	MemorySourceUserConfirmed   = "user_confirmed"
	MemorySourcePatternDetected = "pattern_detected"
)

// ProjectMemory is a long-lived fact OpsPilot has learned about a project —
// recurring failures, confirmed fixes, deploy patterns. Injected into diagnosis
// prompts so the AI gets smarter about each project over time.
type ProjectMemory struct {
	ID               uuid.UUID `json:"id" db:"id"`
	ProjectID        uuid.UUID `json:"project_id" db:"project_id"`
	MemoryType       string    `json:"memory_type" db:"memory_type"`
	Content          string    `json:"content" db:"content"`
	Confidence       float64   `json:"confidence" db:"confidence"`
	Source           string    `json:"source" db:"source"`
	CreatedAt        time.Time `json:"created_at" db:"created_at"`
	LastReferencedAt time.Time `json:"last_referenced_at" db:"last_referenced_at"`
	ReferenceCount   int       `json:"reference_count" db:"reference_count"`
}

// Plan name constants
const (
	PlanFree = "free"
	PlanPro  = "pro"
	PlanTeam = "team"
)

// ─── Organizations & RBAC ────────────────────────────────────────────────────

// Organization is a team workspace. All projects, AWS accounts, alerts, and
// incidents belong to an organization; users access them via membership + role.
// Every user gets a personal organization on first login (created_by themselves).
type Organization struct {
	ID        uuid.UUID `json:"id" db:"id"`
	Name      string    `json:"name" db:"name"`
	Slug      string    `json:"slug" db:"slug"`
	CreatedBy uuid.UUID `json:"created_by" db:"created_by"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
	// Daily-summary delivery config.
	SummaryTime     string `json:"summary_time" db:"summary_time"`         // "HH:MM:SS"
	SummaryTimezone string `json:"summary_timezone" db:"summary_timezone"` // IANA name
	SummaryEnabled  bool   `json:"summary_enabled" db:"summary_enabled"`
	// Role is the requesting user's role in this org — populated by list queries,
	// not a column.
	Role string `json:"role,omitempty" db:"-"`
}

// OrganizationMember links a user to an organization with a role.
type OrganizationMember struct {
	ID        uuid.UUID  `json:"id" db:"id"`
	OrgID     uuid.UUID  `json:"org_id" db:"org_id"`
	UserID    uuid.UUID  `json:"user_id" db:"user_id"`
	Role      string     `json:"role" db:"role"`
	InvitedBy *uuid.UUID `json:"invited_by" db:"invited_by"`
	JoinedAt  time.Time  `json:"joined_at" db:"joined_at"`
	CreatedAt time.Time  `json:"created_at" db:"created_at"`
	// Email is the member's email — populated by the members-list join, not a column.
	Email string `json:"email,omitempty" db:"-"`
}

// OrganizationInvite is a pending invitation to join an org, redeemable via token.
type OrganizationInvite struct {
	ID         uuid.UUID  `json:"id" db:"id"`
	OrgID      uuid.UUID  `json:"org_id" db:"org_id"`
	Email      string     `json:"email" db:"email"`
	Role       string     `json:"role" db:"role"`
	Token      uuid.UUID  `json:"-" db:"token"` // never exposed in list responses
	InvitedBy  uuid.UUID  `json:"invited_by" db:"invited_by"`
	ExpiresAt  time.Time  `json:"expires_at" db:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at" db:"accepted_at"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
}

// Organization role constants. Hierarchical: admin > engineer > viewer.
const (
	RoleAdmin    = "admin"    // invite/remove members, connect AWS, delete projects, change settings
	RoleEngineer = "engineer" // deploy, rollback, scale, terminal, env vars, ack alerts, resolve incidents
	RoleViewer   = "viewer"   // read-only; cannot trigger any action
)

// RoleRank maps a role to a privilege level; a higher rank satisfies any lower
// requirement. Unknown roles rank 0 (no access).
func RoleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleEngineer:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// ValidRole reports whether the string is an assignable org role.
func ValidRole(r string) bool {
	return r == RoleAdmin || r == RoleEngineer || r == RoleViewer
}

// ─── Infrastructure discovery ────────────────────────────────────────────────

// Discovered resource type constants.
const (
	ResourceECSService  = "ecs_service"
	ResourceECSCluster  = "ecs_cluster"
	ResourceRDSInstance = "rds_instance"
	ResourceElastiCache = "elasticache_cluster"
	ResourceLambda      = "lambda_function"
	ResourceS3Bucket    = "s3_bucket"
	ResourceALB         = "alb"
	ResourceCloudFront  = "cloudfront_distribution"
	ResourceSQSQueue    = "sqs_queue"
	ResourceEC2Instance = "ec2_instance"
)

// DiscoveredResource is an AWS resource found in a connected account by the
// discovery scanner. It may be OpsPilot-managed (is_managed, tagged ManagedBy=OpsPilot)
// or pre-existing. project_id is nil until a user assigns it to a project.
type DiscoveredResource struct {
	ID           uuid.UUID      `json:"id" db:"id"`
	OrgID        uuid.UUID      `json:"org_id" db:"org_id"`
	AWSAccountID uuid.UUID      `json:"aws_account_id" db:"aws_account_id"`
	ResourceType string         `json:"resource_type" db:"resource_type"`
	ResourceID   string         `json:"resource_id" db:"resource_id"` // AWS ARN or native ID
	ResourceName string         `json:"resource_name" db:"resource_name"`
	Region       string         `json:"region" db:"region"`
	Metadata     map[string]any `json:"metadata" db:"metadata"`
	Tags         map[string]any `json:"tags" db:"tags"`
	ProjectID    *uuid.UUID     `json:"project_id" db:"project_id"`
	IsManaged    bool           `json:"is_managed" db:"is_managed"`
	FirstSeenAt  time.Time      `json:"first_seen_at" db:"first_seen_at"`
	LastSeenAt   time.Time      `json:"last_seen_at" db:"last_seen_at"`
}

// ─── Slack integration ───────────────────────────────────────────────────────

// SlackIntegration is an org's connected Slack workspace and its channel routing.
// BotToken is encrypted at rest (never exposed in JSON).
type SlackIntegration struct {
	ID                 uuid.UUID  `json:"id" db:"id"`
	OrgID              uuid.UUID  `json:"org_id" db:"org_id"`
	TeamID             string     `json:"team_id" db:"team_id"`
	WorkspaceName      string     `json:"workspace_name" db:"workspace_name"`
	BotToken           string     `json:"-" db:"bot_token"` // encrypted; never exposed
	AlertChannelID     *string    `json:"alert_channel_id" db:"alert_channel_id"`
	AlertChannelName   *string    `json:"alert_channel_name" db:"alert_channel_name"`
	DeployChannelID    *string    `json:"deploy_channel_id" db:"deploy_channel_id"`
	DeployChannelName  *string    `json:"deploy_channel_name" db:"deploy_channel_name"`
	SummaryChannelID   *string    `json:"summary_channel_id" db:"summary_channel_id"`
	SummaryChannelName *string    `json:"summary_channel_name" db:"summary_channel_name"`
	InstalledBy        *uuid.UUID `json:"installed_by" db:"installed_by"`
	CreatedAt          time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at" db:"updated_at"`
}

// DailySummaryRecord is a stored daily summary row (the rich generation logic lives in
// internal/summary). content_json holds the structured metrics; content_markdown the
// rendered briefing.
type DailySummaryRecord struct {
	ID              uuid.UUID      `json:"id" db:"id"`
	OrgID           uuid.UUID      `json:"org_id" db:"org_id"`
	SummaryDate     string         `json:"summary_date" db:"summary_date"` // YYYY-MM-DD
	ContentMarkdown string         `json:"content_markdown" db:"content_markdown"`
	ContentJSON     map[string]any `json:"content_json" db:"content_json"`
	GeneratedAt     time.Time      `json:"generated_at" db:"generated_at"`
	DeliveredSlack  bool           `json:"delivered_slack" db:"delivered_slack"`
	DeliveredEmail  bool           `json:"delivered_email" db:"delivered_email"`
	CreatedAt       time.Time      `json:"created_at" db:"created_at"`
}

// ValidFramework reports whether the string is a supported framework identifier.
func ValidFramework(f string) bool {
	switch f {
	case FrameworkFastAPI, FrameworkFlask, FrameworkDjango, FrameworkPython,
		FrameworkNodeJS, FrameworkExpress, FrameworkNextJS, FrameworkNestJS,
		FrameworkRemix, FrameworkNuxtJS, FrameworkSvelteKit, FrameworkAstro,
		FrameworkReactSPA, FrameworkVite, FrameworkGo, FrameworkRails,
		FrameworkSpring, FrameworkStatic:
		return true
	}
	return false
}
