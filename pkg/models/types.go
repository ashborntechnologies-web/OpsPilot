package models

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID          uuid.UUID  `json:"id" db:"id"`
	ClerkID     string     `json:"clerk_id" db:"clerk_id"`
	Email       string     `json:"email" db:"email"`
	GithubToken *string    `json:"-" db:"github_token"` // never expose in JSON
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
}

type AWSAccount struct {
	ID           uuid.UUID `json:"id" db:"id"`
	UserID       uuid.UUID `json:"user_id" db:"user_id"`
	Label        string    `json:"label" db:"label"`
	AWSAccountID string    `json:"aws_account_id" db:"aws_account_id"`
	IAMRoleARN   string    `json:"iam_role_arn" db:"iam_role_arn"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

type Project struct {
	ID           uuid.UUID  `json:"id" db:"id"`
	UserID       uuid.UUID  `json:"user_id" db:"user_id"`
	Name         string     `json:"name" db:"name"`
	RepoURL      string     `json:"repo_url" db:"repo_url"`
	RepoOwner    string     `json:"repo_owner" db:"repo_owner"`
	RepoName     string     `json:"repo_name" db:"repo_name"`
	Framework    string     `json:"framework" db:"framework"`
	Branch       *string    `json:"branch" db:"branch"`               // nil = default branch
	StartCommand *string    `json:"start_command" db:"start_command"` // user-specified entry command
	AccountID    *uuid.UUID `json:"account_id" db:"account_id"`
	CreatedAt    time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at" db:"updated_at"`
}

// PlatformStack represents the shared infrastructure (VPC, ECS cluster, ALB) provisioned
// once per AWS account × region. Multiple project environments share a single platform stack.
type PlatformStack struct {
	ID                 uuid.UUID  `json:"id" db:"id"`
	AccountID          uuid.UUID  `json:"account_id" db:"account_id"`
	AWSRegion          string     `json:"aws_region" db:"aws_region"`
	StackID            *string    `json:"cloudformation_stack_id" db:"stack_id"`
	StackStatus        string     `json:"stack_status" db:"stack_status"`
	ECSClusterName     *string    `json:"ecs_cluster_name" db:"ecs_cluster_name"`
	ALBArn             *string    `json:"alb_arn" db:"alb_arn"`
	ALBDNS             *string    `json:"alb_dns" db:"alb_dns"`
	ALBListenerArn     *string    `json:"alb_listener_arn" db:"alb_listener_arn"`
	ALBSecurityGroupID *string    `json:"alb_security_group_id" db:"alb_security_group_id"`
	ECSSecurityGroupID *string    `json:"ecs_security_group_id" db:"ecs_security_group_id"`
	SubnetIDs          *string    `json:"subnet_ids" db:"subnet_ids"` // comma-separated
	CreatedAt          time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at" db:"updated_at"`
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
	ALBTargetGroupARN   *string `json:"alb_target_group_arn" db:"alb_target_group_arn"`
	ALBListenerRuleARN  *string `json:"alb_listener_rule_arn" db:"alb_listener_rule_arn"`
	ALBDNS              *string `json:"alb_dns" db:"alb_dns"` // platform ALB DNS (convenience)

	// Legacy single-stack fields — populated for pre-platform-stack environments only.
	// New environments source these from the platform_stacks table via PlatformStackID.
	ECSClusterName     *string `json:"ecs_cluster_name" db:"ecs_cluster_name"`
	ECSSecurityGroupID *string `json:"ecs_security_group_id" db:"ecs_security_group_id"`
	VPCSubnets         *string `json:"vpc_subnets" db:"vpc_subnets"`

	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

type Deployment struct {
	ID            uuid.UUID  `json:"id" db:"id"`
	ProjectID     uuid.UUID  `json:"project_id" db:"project_id"`
	EnvironmentID uuid.UUID  `json:"environment_id" db:"environment_id"`
	CommitSHA     string     `json:"commit_sha" db:"commit_sha"`
	CommitMessage *string    `json:"commit_message" db:"commit_message"`
	ImageURI      *string    `json:"image_uri" db:"image_uri"`
	Status        string     `json:"status" db:"status"` // pending | building | deploying | live | failed | rolled_back
	FailureReason *string    `json:"failure_reason" db:"failure_reason"`
	CreatedAt     time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at" db:"updated_at"`
}

type Incident struct {
	ID            uuid.UUID  `json:"id" db:"id"`
	ProjectID     uuid.UUID  `json:"project_id" db:"project_id"`
	DeploymentID  *uuid.UUID `json:"deployment_id" db:"deployment_id"`
	EnvironmentID *uuid.UUID `json:"environment_id" db:"environment_id"`
	Trigger       string     `json:"trigger" db:"trigger"` // deploy_failure | runtime_anomaly | user_request
	RootCause     *string    `json:"root_cause" db:"root_cause"`
	Resolution    *string    `json:"resolution" db:"resolution"`
	RawLogs       *string    `json:"-" db:"raw_logs"`
	CreatedAt     time.Time  `json:"created_at" db:"created_at"`
}

type Conversation struct {
	ID        uuid.UUID  `json:"id" db:"id"`
	ProjectID uuid.UUID  `json:"project_id" db:"project_id"`
	UserID    uuid.UUID  `json:"user_id" db:"user_id"`
	Role      string     `json:"role" db:"role"` // user | assistant
	Message   string     `json:"message" db:"message"`
	Intent    *string    `json:"intent" db:"intent"`
	Metadata  *string    `json:"metadata" db:"metadata"` // JSONB stored as string
	CreatedAt time.Time  `json:"created_at" db:"created_at"`
}

// Framework constants
const (
	FrameworkFastAPI = "fastapi"
	FrameworkFlask   = "flask"
	FrameworkNodeJS  = "nodejs"
	FrameworkNextJS  = "nextjs"
	FrameworkPython  = "python"
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
	IntentDeploy   = "deploy"
	IntentRedeploy = "redeploy"
	IntentRollback = "rollback"
	IntentScale    = "scale"
	IntentLogs     = "logs"
	IntentHealth   = "health"
	IntentDiagnose = "diagnose"
	IntentUnknown  = "unknown"
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
