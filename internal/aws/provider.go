package aws

import (
	"context"

	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

// AWSProvider is the interface consumed by the deploy and terminal services.
// *Service satisfies this interface; tests inject a mock implementation.
type AWSProvider interface {
	AssumeRoleForEnvironment(ctx context.Context, env *models.Environment) (*ClientBundle, error)
	AssumeRoleForAccount(ctx context.Context, iamRoleARN, externalID, region string) (*ClientBundle, error)

	// CodeBuild
	StartCodeBuildJob(ctx context.Context, clients *ClientBundle, projectID, codeBuildProject, githubToken, owner, repo, commitSHA, imageURI, ecrRegistry, framework, startCommand string) (*StartCodeBuildResult, error)
	WaitForCodeBuild(ctx context.Context, clients *ClientBundle, buildID string, onProgress func(string)) error
	DeleteSSMParameter(ctx context.Context, clients *ClientBundle, name string)

	// ECS task definition + service lifecycle
	RegisterECSTaskDefinition(ctx context.Context, clients *ClientBundle, env *models.Environment, project *models.Project, imageURI string, envVars []ecstypes.KeyValuePair) (string, error)
	EnsureTargetGroup(ctx context.Context, clients *ClientBundle, ps *models.PlatformStack, env *models.Environment, project *models.Project) (string, error)
	EnsureListenerRule(ctx context.Context, clients *ClientBundle, ps *models.PlatformStack, env *models.Environment, tgARN string) (string, error)
	EnsureECSService(ctx context.Context, clients *ClientBundle, env *models.Environment, project *models.Project, taskDefARN, clusterName string, subnets []string, sgID string) error
	WaitForECSServiceStable(ctx context.Context, clients *ClientBundle, clusterName string, env *models.Environment, onProgress func(string)) error
	DescribeECSService(ctx context.Context, clients *ClientBundle, clusterName, serviceName string) (*ServiceHealth, error)
	UpdateServiceDesiredCount(ctx context.Context, clients *ClientBundle, clusterName, serviceName string, desiredCount int32) error
	DeleteECSService(ctx context.Context, clients *ClientBundle, clusterName, serviceName string) error
	GetRunningTask(ctx context.Context, clients *ClientBundle, clusterName, serviceName string) (string, error)
	StartExecSession(ctx context.Context, clients *ClientBundle, clusterName, taskARN, command string) (streamURL, tokenValue string, err error)

	// ALB
	DeleteListenerRule(ctx context.Context, clients *ClientBundle, ruleARN string) error
	DeleteTargetGroup(ctx context.Context, clients *ClientBundle, tgARN string) error

	// ECR
	PurgeECRRepository(ctx context.Context, clients *ClientBundle, repoName string) error

	// CloudWatch Logs
	FetchRecentECSLogs(ctx context.Context, clients *ClientBundle, logGroupName string, limit int32) ([]string, error)
	CreateLogGroupIfNotExists(ctx context.Context, clients *ClientBundle, logGroupName string)

	// Platform + project CloudFormation stacks
	GetOrCreatePlatformStack(ctx context.Context, accountID uuid.UUID, region string) (*models.PlatformStack, bool, error)
	GetPlatformStack(ctx context.Context, id uuid.UUID) (*models.PlatformStack, error)
	DeployPlatformStack(ctx context.Context, clients *ClientBundle, accountID, region string) (string, error)
	WaitForPlatformStackAndPopulate(ctx context.Context, clients *ClientBundle, ps *models.PlatformStack, stackID string, onProgress func(string)) error
	DeployProjectStack(ctx context.Context, clients *ClientBundle, env *models.Environment, project *models.Project) (string, error)
	WaitForProjectStackAndPopulate(ctx context.Context, clients *ClientBundle, env *models.Environment, project *models.Project, stackID string, ps *models.PlatformStack, onProgress func(string)) error
	DeleteProjectStack(ctx context.Context, clients *ClientBundle, stackID string) error

	// Cost
	GetAccountCostSummary(ctx context.Context, projectID string) (*models.CostSummary, error)

	// Conversational mutations
	GetCurrentTaskResources(ctx context.Context, clients *ClientBundle, clusterName, serviceName string) (cpu, memory string, err error)
	UpdateServiceResources(ctx context.Context, clients *ClientBundle, env *models.Environment, project *models.Project, imageURI, cpu, memory string) error

	// PR previews
	CreatePreviewService(ctx context.Context, clients *ClientBundle, stagingEnv *models.Environment, ps *models.PlatformStack, previewEnv *models.Environment, imageURI string, project *models.Project) (listenerRuleARN, targetGroupARN string, err error)
	TeardownPreviewService(ctx context.Context, clients *ClientBundle, previewEnv *models.Environment) error
}
