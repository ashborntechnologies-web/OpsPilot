package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/convdeploy/platform/pkg/middleware"
	"github.com/convdeploy/platform/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Service struct {
	db                *models.DB
	platformAccountID string // AWS account ID of the entity running ConvDeploy
	platformCallerARN string // full ARN of the entity running ConvDeploy (user or role)
	onEnvCreated      func(projectID, environmentID uuid.UUID)
}

// SetOnEnvCreated registers a callback invoked after an environment is created
// and its project already has an AWS account linked. The callback should enqueue
// a provision job and return immediately (non-blocking).
func (s *Service) SetOnEnvCreated(fn func(projectID, environmentID uuid.UUID)) {
	s.onEnvCreated = fn
}

// ClientBundle holds scoped AWS SDK clients for a given assumed-role session.
type ClientBundle struct {
	ECS            *ecs.Client
	ECR            *ecr.Client
	ELB            *elasticloadbalancingv2.Client
	CloudFormation *cloudformation.Client
	CloudWatch     *cloudwatchlogs.Client
	CodeBuild      *codebuild.Client
	Region         string
}

func NewService(db *models.DB, platformAccountID, platformCallerARN string) *Service {
	return &Service{db: db, platformAccountID: platformAccountID, platformCallerARN: platformCallerARN}
}

// AssumeRoleForEnvironment creates scoped AWS SDK clients by assuming the IAM role
// associated with the environment's linked AWS account.
func (s *Service) AssumeRoleForEnvironment(ctx context.Context, env *models.Environment) (*ClientBundle, error) {
	if env.AccountID == nil {
		return nil, fmt.Errorf("environment %s has no AWS account linked", env.ID)
	}

	var iamRoleARN string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT iam_role_arn FROM aws_accounts WHERE id = $1`, env.AccountID,
	).Scan(&iamRoleARN)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS account for environment %s: %w", env.ID, err)
	}

	return s.assumeRole(ctx, iamRoleARN, env.AWSRegion, env.ID.String())
}

// assumeRole is the shared helper that loads config and assumes a role ARN.
func (s *Service) assumeRole(ctx context.Context, iamRoleARN, region, sessionSuffix string) (*ClientBundle, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	stsClient := sts.NewFromConfig(cfg)
	provider := stscreds.NewAssumeRoleProvider(stsClient, iamRoleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = fmt.Sprintf("convdeploy-%s", sessionSuffix)
		o.ExternalID = aws.String("convdeploy")
	})

	assumedCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(aws.NewCredentialsCache(provider)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create assumed-role config: %w", err)
	}

	return &ClientBundle{
		ECS:            ecs.NewFromConfig(assumedCfg),
		ECR:            ecr.NewFromConfig(assumedCfg),
		ELB:            elasticloadbalancingv2.NewFromConfig(assumedCfg),
		CloudFormation: cloudformation.NewFromConfig(assumedCfg),
		CloudWatch:     cloudwatchlogs.NewFromConfig(assumedCfg),
		CodeBuild:      codebuild.NewFromConfig(assumedCfg),
		Region:         region,
	}, nil
}

// ---- CodeBuild ----

// StartCodeBuildJob kicks off a build in the user's AWS account.
// It passes the GitHub token and build parameters as environment variable overrides
// so the statically configured CodeBuild project stays generic.
func (s *Service) StartCodeBuildJob(
	ctx context.Context,
	clients *ClientBundle,
	projectName string,
	githubToken string,
	repoOwner string,
	repoName string,
	commitSHA string,
	imageURI string,
	ecrRegistry string,
	framework string,
	startCommand string,
) (string, error) {
	bs := buildspec()
	dockerfileB64 := base64.StdEncoding.EncodeToString([]byte(defaultDockerfile(framework, startCommand)))

	input := &codebuild.StartBuildInput{
		ProjectName:       aws.String(projectName),
		BuildspecOverride: aws.String(bs),
		EnvironmentVariablesOverride: []cbtypes.EnvironmentVariable{
			{Name: aws.String("GITHUB_TOKEN"), Value: aws.String(githubToken), Type: cbtypes.EnvironmentVariableTypePlaintext},
			{Name: aws.String("REPO_OWNER"), Value: aws.String(repoOwner), Type: cbtypes.EnvironmentVariableTypePlaintext},
			{Name: aws.String("REPO_NAME"), Value: aws.String(repoName), Type: cbtypes.EnvironmentVariableTypePlaintext},
			{Name: aws.String("COMMIT_SHA"), Value: aws.String(commitSHA), Type: cbtypes.EnvironmentVariableTypePlaintext},
			{Name: aws.String("IMAGE_URI"), Value: aws.String(imageURI), Type: cbtypes.EnvironmentVariableTypePlaintext},
			{Name: aws.String("ECR_REGISTRY"), Value: aws.String(ecrRegistry), Type: cbtypes.EnvironmentVariableTypePlaintext},
			{Name: aws.String("DOCKERFILE_B64"), Value: aws.String(dockerfileB64), Type: cbtypes.EnvironmentVariableTypePlaintext},
		},
	}

	out, err := clients.CodeBuild.StartBuild(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to start CodeBuild job: %w", err)
	}

	return aws.ToString(out.Build.Id), nil
}

// WaitForCodeBuild polls the build until it succeeds or fails, calling onProgress for each tick.
func (s *Service) WaitForCodeBuild(ctx context.Context, clients *ClientBundle, buildID string, onProgress func(string)) error {
	for {
		out, err := clients.CodeBuild.BatchGetBuilds(ctx, &codebuild.BatchGetBuildsInput{
			Ids: []string{buildID},
		})
		if err != nil {
			return fmt.Errorf("failed to poll build status: %w", err)
		}
		if len(out.Builds) == 0 {
			return fmt.Errorf("build %s not found", buildID)
		}

		b := out.Builds[0]
		phase := aws.ToString(b.CurrentPhase)

		switch b.BuildStatus {
		case cbtypes.StatusTypeSucceeded:
			onProgress("Build succeeded.")
			return nil
		case cbtypes.StatusTypeFailed, cbtypes.StatusTypeFault, cbtypes.StatusTypeTimedOut, cbtypes.StatusTypeStopped:
			return s.buildFailureError(ctx, clients, b, phase)
		}

		onProgress(fmt.Sprintf("Build in progress: %s...", phase))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// buildFailureError constructs a detailed error from a failed CodeBuild build.
// It extracts the failed phase context and fetches the last 20 lines of build logs from CloudWatch.
func (s *Service) buildFailureError(ctx context.Context, clients *ClientBundle, b cbtypes.Build, currentPhase string) error {
	// Find the phase that failed and its context message
	var phaseMsg string
	for _, p := range b.Phases {
		if p.PhaseStatus == cbtypes.StatusTypeFailed || p.PhaseStatus == cbtypes.StatusTypeTimedOut {
			for _, pctx := range p.Contexts {
				if msg := aws.ToString(pctx.Message); msg != "" {
					phaseMsg = fmt.Sprintf("[%s] %s", p.PhaseType, msg)
					break
				}
			}
			if phaseMsg == "" {
				phaseMsg = fmt.Sprintf("[%s] status=%s", p.PhaseType, b.BuildStatus)
			}
			break
		}
	}
	if phaseMsg == "" {
		phaseMsg = fmt.Sprintf("status=%s phase=%s", b.BuildStatus, currentPhase)
	}

	// Fetch the last 150 log lines from CloudWatch.
	// We need 150 because the last ~10 lines are always CodeBuild phase-status messages;
	// the actual docker build error is further back in the stream.
	var logTail string
	if b.Logs.GroupName != nil && b.Logs.StreamName != nil {
		lines, err := s.FetchLogs(ctx, clients, aws.ToString(b.Logs.GroupName), aws.ToString(b.Logs.StreamName), 150)
		if err == nil && len(lines) > 0 {
			// Strip CodeBuild's own phase-status lines ([Container] 2026/... Phase complete/context/Entering)
			// so the user sees the actual command output, not CodeBuild's bookkeeping.
			var filtered []string
			for _, l := range lines {
				if !strings.HasPrefix(l, "[Container]") {
					filtered = append(filtered, l)
				}
			}
			if len(filtered) > 0 {
				logTail = "\n\n--- build output ---\n" + strings.Join(filtered, "\n")
			} else {
				// Fallback: show raw lines if everything was [Container] prefixed
				logTail = "\n\n--- build log (last 150 lines) ---\n" + strings.Join(lines, "\n")
			}
		}
	}

	return fmt.Errorf("build failed: %s%s", phaseMsg, logTail)
}

// ---- ECS ----

// RegisterECSTaskDefinition creates a new task definition revision for the given image.
func (s *Service) RegisterECSTaskDefinition(
	ctx context.Context,
	clients *ClientBundle,
	env *models.Environment,
	project *models.Project,
	imageURI string,
) (string, error) {
	if env.TaskExecutionRoleARN == nil {
		return "", fmt.Errorf("task execution role ARN not set on environment")
	}
	if env.LogGroupName == nil {
		return "", fmt.Errorf("log group name not set on environment")
	}

	cpu, memory := taskResources(env.Name)
	port := frameworkPort(project.Framework)
	family := fmt.Sprintf("convdeploy-%s", project.ID.String())

	input := &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(family),
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		Cpu:                     aws.String(cpu),
		Memory:                  aws.String(memory),
		ExecutionRoleArn:        aws.String(*env.TaskExecutionRoleARN),
		TaskRoleArn:             aws.String(*env.TaskExecutionRoleARN),
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:      aws.String("app"),
				Image:     aws.String(imageURI),
				Essential: aws.Bool(true),
				PortMappings: []ecstypes.PortMapping{
					{ContainerPort: aws.Int32(port), Protocol: ecstypes.TransportProtocolTcp},
				},
				LogConfiguration: &ecstypes.LogConfiguration{
					LogDriver: ecstypes.LogDriverAwslogs,
					Options: map[string]string{
						"awslogs-group":         *env.LogGroupName,
						"awslogs-region":        clients.Region,
						"awslogs-stream-prefix": "ecs",
					},
				},
			},
		},
	}

	out, err := clients.ECS.RegisterTaskDefinition(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to register task definition: %w", err)
	}

	return aws.ToString(out.TaskDefinition.TaskDefinitionArn), nil
}


// ---- HTTP handlers ----

func (s *Service) HandleCreateEnvironment(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	var req struct {
		Name      string `json:"name" binding:"required"`
		AWSRegion string `json:"aws_region"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name != "staging" && req.Name != "production" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be staging or production"})
		return
	}

	if req.AWSRegion == "" {
		req.AWSRegion = "us-east-1"
	}

	// Look up the project's account_id so we can inherit it on the environment.
	var projectAccountID *uuid.UUID
	err = s.db.Pool.QueryRow(context.Background(),
		`SELECT account_id FROM projects WHERE id = $1`, projectID,
	).Scan(&projectAccountID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	stackStatus := models.StackStatusPending
	if projectAccountID != nil {
		stackStatus = models.StackStatusProvisioning
	}

	env := &models.Environment{
		ProjectID:   projectID,
		Name:        req.Name,
		AWSRegion:   req.AWSRegion,
		AccountID:   projectAccountID,
		StackStatus: stackStatus,
	}

	err = s.db.Pool.QueryRow(context.Background(),
		`INSERT INTO environments (project_id, name, aws_region, account_id, stack_status)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at, updated_at`,
		env.ProjectID, env.Name, env.AWSRegion, env.AccountID, env.StackStatus,
	).Scan(&env.ID, &env.CreatedAt, &env.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create environment"})
		return
	}

	// If project has an account, auto-start provisioning.
	if projectAccountID != nil && s.onEnvCreated != nil {
		s.onEnvCreated(projectID, env.ID)
	}

	c.JSON(http.StatusCreated, env)
}

func (s *Service) HandleListEnvironments(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	rows, err := s.db.Pool.Query(context.Background(),
		`SELECT id, project_id, name, aws_region, account_id,
		        cloudformation_stack_id, stack_status, alb_dns,
		        ecr_repo_uri, ecs_cluster_name, ecs_service_name,
		        codebuild_project_name, task_execution_role_arn, log_group_name,
		        created_at, updated_at
		 FROM environments WHERE project_id = $1 ORDER BY created_at ASC`, projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch environments"})
		return
	}
	defer rows.Close()

	var envs []models.Environment
	for rows.Next() {
		var env models.Environment
		if err := rows.Scan(
			&env.ID, &env.ProjectID, &env.Name, &env.AWSRegion,
			&env.AccountID, &env.CloudFormationStackID,
			&env.StackStatus, &env.ALBDNS,
			&env.ECRRepoURI, &env.ECSClusterName, &env.ECSServiceName,
			&env.CodeBuildProjectName, &env.TaskExecutionRoleARN, &env.LogGroupName,
			&env.CreatedAt, &env.UpdatedAt,
		); err != nil {
			continue
		}
		envs = append(envs, env)
	}

	c.JSON(http.StatusOK, envs)
}

// HandleListAWSAccounts returns all AWS accounts belonging to the current user.
func (s *Service) HandleListAWSAccounts(c *gin.Context) {
	userID, ok := getUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	rows, err := s.db.Pool.Query(context.Background(),
		`SELECT id, user_id, label, aws_account_id, iam_role_arn, created_at, updated_at
		 FROM aws_accounts WHERE user_id = $1 ORDER BY created_at ASC`, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch AWS accounts"})
		return
	}
	defer rows.Close()

	var accounts []models.AWSAccount
	for rows.Next() {
		var a models.AWSAccount
		if err := rows.Scan(&a.ID, &a.UserID, &a.Label, &a.AWSAccountID, &a.IAMRoleARN, &a.CreatedAt, &a.UpdatedAt); err != nil {
			continue
		}
		accounts = append(accounts, a)
	}

	if accounts == nil {
		accounts = []models.AWSAccount{}
	}
	c.JSON(http.StatusOK, accounts)
}

// HandleConnectAWSAccount saves a new AWS account record for the current user.
func (s *Service) HandleConnectAWSAccount(c *gin.Context) {
	userID, ok := getUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	var req struct {
		Label        string `json:"label" binding:"required"`
		AWSAccountID string `json:"aws_account_id" binding:"required"`
		IAMRoleARN   string `json:"iam_role_arn" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify the role can actually be assumed before persisting it.
	if _, err := s.assumeRole(context.Background(), req.IAMRoleARN, "us-east-1", "verify"); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "could not assume provided IAM role — check permissions"})
		return
	}

	account := &models.AWSAccount{
		UserID:       userID,
		Label:        req.Label,
		AWSAccountID: req.AWSAccountID,
		IAMRoleARN:   req.IAMRoleARN,
	}

	err := s.db.Pool.QueryRow(context.Background(),
		`INSERT INTO aws_accounts (user_id, label, aws_account_id, iam_role_arn)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at, updated_at`,
		account.UserID, account.Label, account.AWSAccountID, account.IAMRoleARN,
	).Scan(&account.ID, &account.CreatedAt, &account.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save AWS account"})
		return
	}

	c.JSON(http.StatusCreated, account)
}

// HandleDeleteAWSAccount deletes an AWS account record owned by the current user.
func (s *Service) HandleDeleteAWSAccount(c *gin.Context) {
	userID, ok := getUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	accountID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}

	result, err := s.db.Pool.Exec(context.Background(),
		`DELETE FROM aws_accounts WHERE id = $1 AND user_id = $2`,
		accountID, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete AWS account"})
		return
	}

	if result.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "AWS account not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "AWS account deleted"})
}

// HandleRetryProvision re-triggers provisioning for a failed environment.
func (s *Service) HandleRetryProvision(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
		return
	}

	// Reset status to provisioning.
	_, err = s.db.Pool.Exec(context.Background(),
		`UPDATE environments SET stack_status = $1, updated_at = NOW() WHERE id = $2 AND project_id = $3`,
		models.StackStatusProvisioning, envID, projectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update environment"})
		return
	}

	if s.onEnvCreated != nil {
		s.onEnvCreated(projectID, envID)
	}

	c.JSON(http.StatusOK, gin.H{"message": "provisioning restarted"})
}

// HandleGetBootstrapTemplate returns a fully rendered bootstrap template and a
// ready-to-run AWS CloudShell script. Accepts optional ?region= query param.
func (s *Service) HandleGetBootstrapTemplate(c *gin.Context) {
	region := c.Query("region")
	if region == "" {
		region = "us-east-1"
	}

	if s.platformAccountID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "PLATFORM_AWS_ACCOUNT_ID is not set on the server. " +
				"Run: aws sts get-caller-identity --query Account --output text " +
				"then add PLATFORM_AWS_ACCOUNT_ID=<your-account-id> to your .env and restart.",
		})
		return
	}

	rendered := renderBootstrapTemplate(s.platformAccountID)
	script := renderCloudShellScript(rendered, region, s.platformAccountID, s.platformCallerARN)

	c.JSON(http.StatusOK, gin.H{
		"template":            rendered,
		"script":              script,
		"platform_account_id": s.platformAccountID,
		"region":              region,
	})
}

// renderBootstrapTemplate returns the bootstrap YAML with the ConvDeployAccountId
// parameter replaced by the actual platform account ID, so users get a complete template.
func renderBootstrapTemplate(platformAccountID string) string {
	// Replace the parameter reference and remove the Parameters block.
	rendered := strings.ReplaceAll(
		BootstrapTemplate,
		"!Sub 'arn:aws:iam::${ConvDeployAccountId}:root'",
		fmt.Sprintf("'arn:aws:iam::%s:root'", platformAccountID),
	)
	// Strip the Parameters block — no longer needed now that we've hardcoded the value.
	start := strings.Index(rendered, "\nParameters:")
	end := strings.Index(rendered, "\nResources:")
	if start != -1 && end != -1 {
		rendered = rendered[:start] + rendered[end:]
	}
	return rendered
}

// renderCloudShellScript builds a self-contained bash script the user can paste
// directly into AWS CloudShell. It creates the bootstrap stack and — when the
// platform is running in the same AWS account as the user — also grants
// sts:AssumeRole to the platform entity. Cross-account AssumeRole doesn't need
// this (the trust policy alone is sufficient), but same-account does.
func renderCloudShellScript(template, region, platformAccountID, platformCallerARN string) string {
	// Same-account grant block — only emitted when caller ARN is known and the
	// platform account matches. The script double-checks at runtime with
	// sts:GetCallerIdentity so it's safe to include unconditionally.
	sameAccountBlock := ""
	if platformCallerARN != "" && platformAccountID != "" {
		sameAccountBlock = fmt.Sprintf(`
# ── Same-account: grant ConvDeploy permission to assume this role ────────────
# When the platform runs in the same AWS account as this deployment, AWS requires
# an explicit sts:AssumeRole permission on the calling entity (trust policy alone
# is not sufficient for same-account). Cross-account users: this block is skipped.
CALLER_ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
PLATFORM_ACCOUNT="%s"
PLATFORM_ARN="%s"

if [ "$CALLER_ACCOUNT" = "$PLATFORM_ACCOUNT" ]; then
  RESOURCE_PART=$(echo "$PLATFORM_ARN" | cut -d: -f6)
  PRINCIPAL_TYPE=$(echo "$RESOURCE_PART" | cut -d/ -f1)
  PRINCIPAL_NAME=$(echo "$RESOURCE_PART" | cut -d/ -f2-)
  POLICY="{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"sts:AssumeRole\",\"Resource\":\"arn:aws:iam::${CALLER_ACCOUNT}:role/ConvDeployPlatformRole\"}]}"

  if [ "$PRINCIPAL_TYPE" = "user" ]; then
    aws iam put-user-policy \
      --user-name "$PRINCIPAL_NAME" \
      --policy-name "ConvDeployPlatformAccess" \
      --policy-document "$POLICY"
    echo "Granted sts:AssumeRole to IAM user: $PRINCIPAL_NAME"
  elif [ "$PRINCIPAL_TYPE" = "assumed-role" ]; then
    ROLE_NAME=$(echo "$PRINCIPAL_NAME" | cut -d/ -f1)
    aws iam put-role-policy \
      --role-name "$ROLE_NAME" \
      --policy-name "ConvDeployPlatformAccess" \
      --policy-document "$POLICY"
    echo "Granted sts:AssumeRole to IAM role: $ROLE_NAME"
  fi
fi
`, platformAccountID, platformCallerARN)
	}

	return fmt.Sprintf(`#!/bin/bash
# ConvDeploy bootstrap — creates the platform access role in your AWS account.
# Paste this entire script into AWS CloudShell and press Enter.
# CloudShell is available at: https://console.aws.amazon.com/cloudshell/

set -e
REGION="%s"
STACK_NAME="convdeploy-bootstrap"

echo "Writing bootstrap template..."
cat > /tmp/convdeploy-bootstrap.yaml << 'YAML_EOF'
%s
YAML_EOF

echo "Deploying CloudFormation stack (takes ~2 min)..."
aws cloudformation deploy \
  --region "$REGION" \
  --stack-name "$STACK_NAME" \
  --template-file /tmp/convdeploy-bootstrap.yaml \
  --capabilities CAPABILITY_NAMED_IAM \
  --no-fail-on-empty-changeset
%s
echo ""
echo "Done! Copy the Role ARN below and paste it into ConvDeploy:"
echo ""
aws cloudformation describe-stacks \
  --region "$REGION" \
  --stack-name "$STACK_NAME" \
  --query "Stacks[0].Outputs[?OutputKey=='RoleArn'].OutputValue" \
  --output text
`, region, strings.TrimSpace(template), sameAccountBlock)
}

// GetPlatformStack fetches a platform stack record by ID.
func (s *Service) GetPlatformStack(ctx context.Context, id uuid.UUID) (*models.PlatformStack, error) {
	var ps models.PlatformStack
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, account_id, aws_region, stack_id, stack_status,
		        ecs_cluster_name, alb_arn, alb_dns, alb_listener_arn,
		        alb_security_group_id, ecs_security_group_id, subnet_ids,
		        created_at, updated_at
		 FROM platform_stacks WHERE id = $1`, id,
	).Scan(
		&ps.ID, &ps.AccountID, &ps.AWSRegion, &ps.StackID, &ps.StackStatus,
		&ps.ECSClusterName, &ps.ALBArn, &ps.ALBDNS, &ps.ALBListenerArn,
		&ps.ALBSecurityGroupID, &ps.ECSSecurityGroupID, &ps.SubnetIDs,
		&ps.CreatedAt, &ps.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &ps, nil
}

// GetOrCreatePlatformStack finds an existing platform stack for the account+region or
// creates a new record in "pending" state.
func (s *Service) GetOrCreatePlatformStack(ctx context.Context, accountID uuid.UUID, region string) (*models.PlatformStack, bool, error) {
	var ps models.PlatformStack
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, account_id, aws_region, stack_id, stack_status,
		        ecs_cluster_name, alb_arn, alb_dns, alb_listener_arn,
		        alb_security_group_id, ecs_security_group_id, subnet_ids,
		        created_at, updated_at
		 FROM platform_stacks WHERE account_id = $1 AND aws_region = $2`,
		accountID, region,
	).Scan(
		&ps.ID, &ps.AccountID, &ps.AWSRegion, &ps.StackID, &ps.StackStatus,
		&ps.ECSClusterName, &ps.ALBArn, &ps.ALBDNS, &ps.ALBListenerArn,
		&ps.ALBSecurityGroupID, &ps.ECSSecurityGroupID, &ps.SubnetIDs,
		&ps.CreatedAt, &ps.UpdatedAt,
	)
	if err == nil {
		return &ps, false, nil // already exists
	}

	// Create new platform stack record.
	err = s.db.Pool.QueryRow(ctx,
		`INSERT INTO platform_stacks (account_id, aws_region, stack_status)
		 VALUES ($1, $2, $3)
		 RETURNING id, created_at, updated_at`,
		accountID, region, models.StackStatusPending,
	).Scan(&ps.ID, &ps.CreatedAt, &ps.UpdatedAt)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create platform stack record: %w", err)
	}

	ps.AccountID = accountID
	ps.AWSRegion = region
	ps.StackStatus = models.StackStatusPending

	return &ps, true, nil
}

// EnsureTargetGroup creates a Target Group for the project-environment if one doesn't exist,
// or returns the existing one. The TG is attached to the shared platform ALB.
// Called at deploy time (not provision time) so non-HTTP workloads can skip it.
func (s *Service) EnsureTargetGroup(
	ctx context.Context,
	clients *ClientBundle,
	ps *models.PlatformStack,
	env *models.Environment,
	project *models.Project,
) (string, error) {
	// Re-use existing TG if already created.
	if env.ALBTargetGroupARN != nil && *env.ALBTargetGroupARN != "" {
		return *env.ALBTargetGroupARN, nil
	}

	if ps.ALBArn == nil {
		return "", fmt.Errorf("platform stack ALB ARN not set")
	}

	port := int32(frameworkPort(project.Framework))
	tgName := tgName(project.ID.String(), env.Name)

	// Check if the TG already exists in AWS (e.g. after a previous failed deploy).
	descOut, err := clients.ELB.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
		Names: []string{tgName},
	})
	if err == nil && len(descOut.TargetGroups) > 0 {
		tgARN := aws.ToString(descOut.TargetGroups[0].TargetGroupArn)
		s.saveTargetGroupARN(ctx, env.ID, tgARN)
		return tgARN, nil
	}

	// Derive the VPC ID from the platform ALB.
	albDesc, err := clients.ELB.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []string{*ps.ALBArn},
	})
	if err != nil || len(albDesc.LoadBalancers) == 0 {
		return "", fmt.Errorf("failed to describe platform ALB: %w", err)
	}
	vpcID := aws.ToString(albDesc.LoadBalancers[0].VpcId)

	out, err := clients.ELB.CreateTargetGroup(ctx, &elasticloadbalancingv2.CreateTargetGroupInput{
		Name:                       aws.String(tgName),
		Port:                       aws.Int32(port),
		Protocol:                   elbtypes.ProtocolEnumHttp,
		VpcId:                      aws.String(vpcID),
		TargetType:                 elbtypes.TargetTypeEnumIp,
		HealthCheckPath:            aws.String("/"),
		HealthCheckIntervalSeconds: aws.Int32(30),
		HealthCheckTimeoutSeconds:  aws.Int32(5),
		HealthyThresholdCount:      aws.Int32(2),
		UnhealthyThresholdCount:    aws.Int32(3),
		Matcher:                    &elbtypes.Matcher{HttpCode: aws.String("200-499")},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create target group: %w", err)
	}

	tgARN := aws.ToString(out.TargetGroups[0].TargetGroupArn)
	s.saveTargetGroupARN(ctx, env.ID, tgARN)
	return tgARN, nil
}

func (s *Service) saveTargetGroupARN(ctx context.Context, envID uuid.UUID, tgARN string) {
	s.db.Pool.Exec(ctx, `UPDATE environments SET alb_target_group_arn = $1, updated_at = NOW() WHERE id = $2`, tgARN, envID)
}

// EnsureListenerRule creates or updates the ALB listener rule for this project-environment.
// Uses a path-pattern rule with a deterministic priority. Callers pass the TG ARN from EnsureTargetGroup.
func (s *Service) EnsureListenerRule(
	ctx context.Context,
	clients *ClientBundle,
	ps *models.PlatformStack,
	env *models.Environment,
	tgARN string,
) (string, error) {
	if ps.ALBListenerArn == nil {
		return "", fmt.Errorf("platform stack ALB listener ARN not set")
	}

	// Deterministic priority per env — unique enough for the number of envs we expect.
	priority := int32(crc32.ChecksumIEEE([]byte(env.ID.String()))%49000) + 1000

	// Check if a rule for this TG already exists on the listener.
	existingRules, err := clients.ELB.DescribeRules(ctx, &elasticloadbalancingv2.DescribeRulesInput{
		ListenerArn: ps.ALBListenerArn,
	})
	if err == nil {
		for _, r := range existingRules.Rules {
			for _, a := range r.Actions {
				if a.TargetGroupArn != nil && *a.TargetGroupArn == tgARN {
					ruleARN := aws.ToString(r.RuleArn)
					s.saveListenerRuleARN(ctx, env.ID, ruleARN)
					return ruleARN, nil
				}
			}
		}
	}

	out, err := clients.ELB.CreateRule(ctx, &elasticloadbalancingv2.CreateRuleInput{
		ListenerArn: ps.ALBListenerArn,
		Priority:    aws.Int32(priority),
		Conditions: []elbtypes.RuleCondition{
			{
				Field:  aws.String("path-pattern"),
				Values: []string{"/*"},
			},
		},
		Actions: []elbtypes.Action{
			{
				Type:           elbtypes.ActionTypeEnumForward,
				TargetGroupArn: aws.String(tgARN),
			},
		},
	})
	if err != nil {
		// Priority collision — retry with a different value.
		if strings.Contains(err.Error(), "PriorityInUse") {
			priority += 500
			out, err = clients.ELB.CreateRule(ctx, &elasticloadbalancingv2.CreateRuleInput{
				ListenerArn: ps.ALBListenerArn,
				Priority:    aws.Int32(priority),
				Conditions:  []elbtypes.RuleCondition{{Field: aws.String("path-pattern"), Values: []string{"/*"}}},
				Actions:     []elbtypes.Action{{Type: elbtypes.ActionTypeEnumForward, TargetGroupArn: aws.String(tgARN)}},
			})
		}
		if err != nil {
			return "", fmt.Errorf("failed to create listener rule: %w", err)
		}
	}

	ruleARN := aws.ToString(out.Rules[0].RuleArn)
	s.saveListenerRuleARN(ctx, env.ID, ruleARN)
	return ruleARN, nil
}

func (s *Service) saveListenerRuleARN(ctx context.Context, envID uuid.UUID, ruleARN string) {
	s.db.Pool.Exec(ctx, `UPDATE environments SET alb_listener_rule_arn = $1, updated_at = NOW() WHERE id = $2`, ruleARN, envID)
}

// EnsureECSService creates the ECS service on first deploy, or updates it on subsequent deploys.
// clusterName, subnets, and sgID come from the platform stack (new model) or from env fields (legacy model).
func (s *Service) EnsureECSService(
	ctx context.Context,
	clients *ClientBundle,
	env *models.Environment,
	project *models.Project,
	taskDefARN string,
	clusterName string,
	subnets []string,
	sgID string,
) error {
	if env.ECSServiceName == nil {
		return fmt.Errorf("ECS service name not set on environment")
	}
	if env.ALBTargetGroupARN == nil {
		return fmt.Errorf("target group not set — EnsureTargetGroup must run before EnsureECSService")
	}

	port := frameworkPort(project.Framework)

	out, descErr := clients.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []string{*env.ECSServiceName},
	})

	serviceActive := descErr == nil &&
		len(out.Services) > 0 &&
		out.Services[0].Status != nil &&
		*out.Services[0].Status != "INACTIVE"

	if serviceActive {
		_, err := clients.ECS.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster:              aws.String(clusterName),
			Service:              aws.String(*env.ECSServiceName),
			TaskDefinition:       aws.String(taskDefARN),
			ForceNewDeployment:   true,
			EnableExecuteCommand: aws.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("failed to update ECS service: %w", err)
		}
		return nil
	}

	desiredCount := int32(1)
	if env.Name == "production" {
		desiredCount = 2
	}

	_, err := clients.ECS.CreateService(ctx, &ecs.CreateServiceInput{
		Cluster:              aws.String(clusterName),
		ServiceName:          aws.String(*env.ECSServiceName),
		TaskDefinition:       aws.String(taskDefARN),
		DesiredCount:         aws.Int32(desiredCount),
		LaunchType:           ecstypes.LaunchTypeFargate,
		EnableExecuteCommand: true,
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        subnets,
				SecurityGroups: []string{sgID},
				AssignPublicIp: ecstypes.AssignPublicIpEnabled,
			},
		},
		LoadBalancers: []ecstypes.LoadBalancer{
			{
				TargetGroupArn: aws.String(*env.ALBTargetGroupARN),
				ContainerName:  aws.String("app"),
				ContainerPort:  aws.Int32(port),
			},
		},
		DeploymentConfiguration: &ecstypes.DeploymentConfiguration{
			MaximumPercent:        aws.Int32(200),
			MinimumHealthyPercent: aws.Int32(50),
		},
		SchedulingStrategy: ecstypes.SchedulingStrategyReplica,
	})
	if err != nil {
		return fmt.Errorf("failed to create ECS service: %w", err)
	}

	return nil
}

// WaitForECSServiceStable polls until the ECS service deployment is stable.
func (s *Service) WaitForECSServiceStable(ctx context.Context, clients *ClientBundle, clusterName string, env *models.Environment, onProgress func(string)) error {
	if env.ECSServiceName == nil {
		return fmt.Errorf("ECS service name not set on environment")
	}

	for {
		out, err := clients.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster:  aws.String(clusterName),
			Services: []string{*env.ECSServiceName},
		})
		if err != nil {
			return fmt.Errorf("failed to describe ECS service: %w", err)
		}
		if len(out.Services) == 0 {
			return fmt.Errorf("ECS service not found")
		}

		svc := out.Services[0]

		for _, d := range svc.Deployments {
			if d.Status == nil || *d.Status != "PRIMARY" {
				continue
			}
			switch d.RolloutState {
			case ecstypes.DeploymentRolloutStateCompleted:
				onProgress(fmt.Sprintf("Service stable. Running tasks: %d/%d.", d.RunningCount, d.DesiredCount))
				return nil
			case ecstypes.DeploymentRolloutStateFailed:
				return fmt.Errorf("ECS deployment failed: %s", aws.ToString(d.RolloutStateReason))
			default:
				onProgress(fmt.Sprintf("Deploying: %d/%d tasks running...", d.RunningCount, d.DesiredCount))
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// tgName returns the Target Group name for a project-environment. Max 32 chars (ALB limit).
func tgName(projectID, envName string) string {
	short := projectID
	if len(short) > 8 {
		short = short[:8]
	}
	shortEnv := envName
	if len(shortEnv) > 4 {
		shortEnv = shortEnv[:4]
	}
	return fmt.Sprintf("cd-%s-%s-tg", short, shortEnv) // max 18 chars
}

// FetchRecentECSLogs fetches the most recent application logs across all ECS task streams
// in a log group. Uses FilterLogEvents with the "ecs/app/" stream prefix so it works even
// when tasks are replaced (new stream per task).
func (s *Service) FetchRecentECSLogs(ctx context.Context, clients *ClientBundle, logGroupName string, limit int32) ([]string, error) {
	out, err := clients.CloudWatch.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:        aws.String(logGroupName),
		LogStreamNamePrefix: aws.String("ecs/app/"),
		Limit:               aws.Int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch ECS logs: %w", err)
	}

	lines := make([]string, 0, len(out.Events))
	for _, e := range out.Events {
		if e.Message != nil {
			lines = append(lines, *e.Message)
		}
	}
	return lines, nil
}

// FetchLogs pulls the most recent CloudWatch log events for a given log group and stream.
func (s *Service) FetchLogs(ctx context.Context, clients *ClientBundle, logGroupName, logStreamName string, limit int32) ([]string, error) {
	out, err := clients.CloudWatch.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
		Limit:         aws.Int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch logs: %w", err)
	}

	var lines []string
	for _, event := range out.Events {
		if event.Message != nil {
			lines = append(lines, *event.Message)
		}
	}

	return lines, nil
}

// ---- private helpers ----

// getUserID extracts the authenticated user UUID from the Gin context.
func getUserID(c *gin.Context) (uuid.UUID, bool) {
	return middleware.GetUserID(c)
}

// buildspec returns the inline buildspec.yml passed to every CodeBuild run.
// All variable values are injected as environment variable overrides at StartBuild time.
func buildspec() string {
	return `version: 0.2
phases:
  install:
    commands:
      - git clone https://x-access-token:$GITHUB_TOKEN@github.com/$REPO_OWNER/$REPO_NAME.git /tmp/src
      - cd /tmp/src && git checkout $COMMIT_SHA
  pre_build:
    commands:
      - aws ecr get-login-password --region $AWS_DEFAULT_REGION | docker login --username AWS --password-stdin $ECR_REGISTRY
      - if [ ! -f /tmp/src/Dockerfile ]; then printf '%s' "$DOCKERFILE_B64" | base64 -d > /tmp/src/Dockerfile; fi
  build:
    commands:
      - cd /tmp/src && docker build -t $IMAGE_URI .
  post_build:
    commands:
      - |
        if [ "$CODEBUILD_BUILD_SUCCEEDING" = "1" ]; then
          docker push $IMAGE_URI
        else
          echo "Build phase failed — skipping push."
        fi`
}

// defaultStartCommand returns the framework's default start command.
// This is used as a placeholder in the UI and as a fallback when no start_command is saved.
func defaultStartCommand(framework string) string {
	switch framework {
	case "fastapi":
		return "uvicorn main:app --host 0.0.0.0 --port 8000"
	case "flask":
		return "gunicorn main:app -b 0.0.0.0:8000"
	case "python":
		return "python main.py"
	case "nodejs":
		return "node index.js"
	case "nextjs":
		return "node server.js"
	default:
		return "./start.sh"
	}
}

// defaultDockerfile generates a Dockerfile for the given framework using the provided
// start command. If startCommand is empty the framework default is used.
// The start command is run via `sh -c` so arbitrary shell syntax works.
func defaultDockerfile(framework, startCommand string) string {
	if startCommand == "" {
		startCommand = defaultStartCommand(framework)
	}
	port := frameworkPort(framework)
	cmd := fmt.Sprintf(`CMD ["sh", "-c", %q]`, startCommand)

	// Use ECR Public Gallery mirrors to avoid Docker Hub unauthenticated pull rate limits.
	// These are identical images served from AWS infrastructure — no auth required from CodeBuild.
	switch framework {
	case "fastapi", "flask":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/python:3.11-slim
WORKDIR /app
COPY requirements*.txt pyproject.toml* ./
RUN pip install --no-cache-dir -r requirements.txt 2>/dev/null || pip install --no-cache-dir .
COPY . .
EXPOSE %d
%s`, port, cmd)

	case "python":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/python:3.11-slim
WORKDIR /app
COPY requirements*.txt pyproject.toml* ./
RUN pip install --no-cache-dir -r requirements.txt 2>/dev/null || pip install --no-cache-dir -e . 2>/dev/null || true
COPY . .
EXPOSE %d
%s`, port, cmd)

	case "nodejs":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/node:20-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci --only=production
COPY . .
EXPOSE %d
%s`, port, cmd)

	case "nextjs":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/node:20-alpine AS deps
WORKDIR /app
COPY package*.json ./
RUN npm ci

FROM public.ecr.aws/docker/library/node:20-alpine AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN npm run build

FROM public.ecr.aws/docker/library/node:20-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
COPY --from=builder /app/public ./public
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/.next/static ./.next/static
ENV PORT=%d
EXPOSE %d
%s`, port, port, cmd)

	default:
		return fmt.Sprintf(`FROM public.ecr.aws/ubuntu/ubuntu:22.04
WORKDIR /app
COPY . .
EXPOSE %d
%s`, port, cmd)
	}
}

// frameworkPort returns the container port for each supported framework.
func frameworkPort(framework string) int32 {
	switch framework {
	case "nodejs", "nextjs":
		return 3000
	default:
		return 8000
	}
}

// taskResources returns CPU and memory strings for Fargate based on environment name.
func taskResources(envName string) (cpu, memory string) {
	if envName == "production" {
		return "512", "1024"
	}
	return "256", "512"
}
