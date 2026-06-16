package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/awstags"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type Service struct {
	db                 *models.DB
	platformAccountID  string          // AWS account ID of the entity running ConvDeploy
	platformCallerARN  string          // full ARN of the entity running ConvDeploy (user or role)
	events             *events.Service // optional; set via SetEvents for audit events
	onEnvCreated       func(projectID, environmentID uuid.UUID)
	onAccountConnected func(orgID, accountID uuid.UUID) // optional; triggers initial discovery scan
}

// SetEvents injects the operational-event service so account-level actions
// (e.g. external_id.generated) can be audited. Optional — Emit calls are nil-guarded.
func (s *Service) SetEvents(e *events.Service) {
	s.events = e
}

// LegacyExternalID is the shared external ID used before per-tenant external IDs existed.
// Accounts connected before that change keep this value (DB default) so their already
// deployed bootstrap roles continue to be assumable.
const LegacyExternalID = "convdeploy"

// externalIDPlaceholder is the token in BootstrapTemplate replaced at render time with
// the connection's actual external ID.
const externalIDPlaceholder = "CONVDEPLOY_EXTERNAL_ID"

// NewExternalID returns a fresh per-connection STS external ID. Embedding a unique value
// in each role's trust policy prevents the confused-deputy attack: a tenant cannot assume
// another tenant's role even if they learn its ARN, because they cannot know its external ID.
func NewExternalID() string {
	return "convdeploy-" + uuid.NewString()
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
	Metrics        *cloudwatch.Client
	CodeBuild      *codebuild.Client
	SSM            *ssm.Client
	CostExplorer   *costexplorer.Client // always us-east-1 (global service)
	Region         string
}

func NewService(db *models.DB, platformAccountID, platformCallerARN string) *Service {
	return &Service{
		db:                db,
		platformAccountID: platformAccountID,
		platformCallerARN: platformCallerARN,
	}
}

// AssumeRoleForEnvironment creates scoped AWS SDK clients by assuming the IAM role
// associated with the environment's linked AWS account, using that account's stored
// per-tenant external ID.
func (s *Service) AssumeRoleForEnvironment(ctx context.Context, env *models.Environment) (*ClientBundle, error) {
	if env.AccountID == nil {
		return nil, fmt.Errorf("environment %s has no AWS account linked", env.ID)
	}

	var iamRoleARN, externalID string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT iam_role_arn, external_id FROM aws_accounts WHERE id = $1`, env.AccountID,
	).Scan(&iamRoleARN, &externalID)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS account for environment %s: %w", env.ID, err)
	}
	if externalID == "" {
		externalID = LegacyExternalID
	}

	return s.assumeRole(ctx, iamRoleARN, env.AWSRegion, env.ID.String(), externalID)
}

// explainAssumeRoleError turns the raw STS AccessDenied into an actionable message.
// The two real-world causes are (a) external-ID mismatch after the customer re-ran the
// setup script, and (b) same-account setups where the platform caller is missing the
// sts:AssumeRole identity policy (trust policy alone is insufficient within one account).
func (s *Service) explainAssumeRoleError(err error, iamRoleARN string) error {
	if !strings.Contains(err.Error(), "AccessDenied") {
		return fmt.Errorf("failed to assume IAM role %s: %w", iamRoleARN, err)
	}

	hint := "the role's trust policy may no longer match this connection — if you re-ran the setup script, reconnect the AWS account in ConvDeploy so the external ID matches"
	// Same-account: the platform caller needs an identity policy, which the setup
	// script normally grants. Spell out the exact missing grant.
	if s.platformAccountID != "" && strings.Contains(iamRoleARN, ":"+s.platformAccountID+":") {
		hint = fmt.Sprintf(
			"platform and target role are in the same AWS account (%s), which requires an explicit sts:AssumeRole grant on the platform identity (%s). Re-run the latest setup script, or attach a policy allowing sts:AssumeRole on %s",
			s.platformAccountID, s.platformCallerARN, iamRoleARN,
		)
	}
	return fmt.Errorf("AWS denied sts:AssumeRole on %s: %s (%w)", iamRoleARN, hint, err)
}

// assumeRole is the shared helper that loads config and assumes a role ARN with the
// given STS external ID.
func (s *Service) assumeRole(ctx context.Context, iamRoleARN, region, sessionSuffix, externalID string) (*ClientBundle, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	stsClient := sts.NewFromConfig(cfg)
	provider := stscreds.NewAssumeRoleProvider(stsClient, iamRoleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = fmt.Sprintf("convdeploy-%s", sessionSuffix)
		o.ExternalID = aws.String(externalID)
	})

	// Verify the role can be assumed NOW. The credentials provider is lazy, so
	// without this check an AssumeRole failure surfaces later buried inside an
	// unrelated ECS/CloudFormation operation error.
	if _, err := provider.Retrieve(ctx); err != nil {
		return nil, s.explainAssumeRoleError(err, iamRoleARN)
	}

	assumedCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(aws.NewCredentialsCache(provider)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create assumed-role config: %w", err)
	}

	// Cost Explorer is a global service — must always use us-east-1 regardless of env region.
	ceCfg, _ := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.NewCredentialsCache(provider)),
	)

	return &ClientBundle{
		ECS:            ecs.NewFromConfig(assumedCfg),
		ECR:            ecr.NewFromConfig(assumedCfg),
		ELB:            elasticloadbalancingv2.NewFromConfig(assumedCfg),
		CloudFormation: cloudformation.NewFromConfig(assumedCfg),
		CloudWatch:     cloudwatchlogs.NewFromConfig(assumedCfg),
		Metrics:        cloudwatch.NewFromConfig(assumedCfg),
		CodeBuild:      codebuild.NewFromConfig(assumedCfg),
		SSM:            ssm.NewFromConfig(assumedCfg),
		CostExplorer:   costexplorer.NewFromConfig(ceCfg),
		Region:         region,
	}, nil
}

// ---- CodeBuild ----

// githubTokenParamName returns the SSM parameter path that holds the GitHub token for a
// given project+commit build. Scoped under /convdeploy/ so the IAM policies can grant
// narrowly. Deleted after the build completes.
func githubTokenParamName(projectID, commitSHA string) string {
	sha := commitSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	return fmt.Sprintf("/convdeploy/%s/%s/github-token", projectID, sha)
}

// StartCodeBuildResult is returned by StartCodeBuildJob.
type StartCodeBuildResult struct {
	BuildID string
	// TokenParamName is the SSM parameter holding the GitHub token, if the secure path was
	// used. Empty when the token was passed inline (legacy fallback). The caller deletes it
	// after the build finishes.
	TokenParamName string
	// SecretStored is true when the token was stored in SSM (secure path), false on fallback.
	SecretStored bool
}

// StartCodeBuildJob kicks off a build in the user's AWS account. The GitHub token is
// stored in SSM Parameter Store as a SecureString and referenced by name (PARAMETER_STORE
// env override) so it never appears in the StartBuild request or CodeBuild's plaintext env.
// If storing the secret fails (e.g. a pre-upgrade bootstrap role without ssm:PutParameter),
// it falls back to passing the token inline so existing accounts keep deploying.
func (s *Service) StartCodeBuildJob(
	ctx context.Context,
	clients *ClientBundle,
	projectID string,
	projectName string,
	githubToken string,
	repoOwner string,
	repoName string,
	commitSHA string,
	imageURI string,
	ecrRegistry string,
	framework string,
	startCommand string,
) (*StartCodeBuildResult, error) {
	bs := buildspec()
	dockerfileB64 := base64.StdEncoding.EncodeToString([]byte(defaultDockerfile(framework, startCommand)))

	// The build always reads the token from $GITHUB_TOKEN; only how we inject it differs.
	tokenEnv := cbtypes.EnvironmentVariable{
		Name:  aws.String("GITHUB_TOKEN"),
		Value: aws.String(githubToken),
		Type:  cbtypes.EnvironmentVariableTypePlaintext,
	}

	paramName := githubTokenParamName(projectID, commitSHA)
	secretStored := false
	if err := s.putSecureParameter(ctx, clients, paramName, githubToken); err != nil {
		// Legacy role without SSM permissions — fall back to inline token so deploys keep
		// working. The operator should re-run the bootstrap stack to enable the secure path.
		slog.Warn(fmt.Sprintf("WARNING: SSM PutParameter failed (%v) — falling back to inline GitHub token for project %s. Re-run the bootstrap stack to enable the secure path.", err, projectID))
	} else {
		secretStored = true
		tokenEnv = cbtypes.EnvironmentVariable{
			Name:  aws.String("GITHUB_TOKEN"),
			Value: aws.String(paramName),
			Type:  cbtypes.EnvironmentVariableTypeParameterStore,
		}
	}

	input := &codebuild.StartBuildInput{
		ProjectName:       aws.String(projectName),
		BuildspecOverride: aws.String(bs),
		EnvironmentVariablesOverride: []cbtypes.EnvironmentVariable{
			tokenEnv,
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
		// Don't leak the secret if the build failed to start.
		if secretStored {
			s.DeleteSSMParameter(ctx, clients, paramName)
		}
		return nil, fmt.Errorf("failed to start CodeBuild job: %w", err)
	}

	res := &StartCodeBuildResult{BuildID: aws.ToString(out.Build.Id), SecretStored: secretStored}
	if secretStored {
		res.TokenParamName = paramName
	}
	return res, nil
}

// putSecureParameter writes a SecureString SSM parameter, overwriting any existing value.
func (s *Service) putSecureParameter(ctx context.Context, clients *ClientBundle, name, value string) error {
	_, err := clients.SSM.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(name),
		Value:     aws.String(value),
		Type:      ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	return err
}

// DeleteSSMParameter removes an SSM parameter. Best-effort: errors are logged, not returned,
// since cleanup failure must not fail a deploy.
func (s *Service) DeleteSSMParameter(ctx context.Context, clients *ClientBundle, name string) {
	if name == "" {
		return
	}
	_, err := clients.SSM.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)})
	if err != nil && !strings.Contains(err.Error(), "ParameterNotFound") {
		slog.Warn(fmt.Sprintf("WARNING: failed to delete SSM parameter %s: %v", name, err))
	}
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
// envVars are injected into the container definition; pass nil to run with no env vars.
func (s *Service) RegisterECSTaskDefinition(
	ctx context.Context,
	clients *ClientBundle,
	env *models.Environment,
	project *models.Project,
	imageURI string,
	envVars []ecstypes.KeyValuePair,
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

	tags := awstags.ToECS(awstags.BuildResourceTags(project.ID.String(), env.Name, s.platformAccountID))
	input := &ecs.RegisterTaskDefinitionInput{
		Tags:                    tags,
		Family:                  aws.String(family),
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		Cpu:                     aws.String(cpu),
		Memory:                  aws.String(memory),
		ExecutionRoleArn:        aws.String(*env.TaskExecutionRoleARN),
		TaskRoleArn:             aws.String(*env.TaskExecutionRoleARN),
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:        aws.String("app"),
				Image:       aws.String(imageURI),
				Essential:   aws.Bool(true),
				Environment: envVars,
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

var (
	awsAccountIDRe = regexp.MustCompile(`^\d{12}$`)
	iamRoleARNRe   = regexp.MustCompile(`^arn:aws:iam::(\d{12}):role/[\w+=,.@/-]+$`)
	acmCertARNRe   = regexp.MustCompile(`^arn:aws:acm:[a-z0-9-]+:\d{12}:certificate/[\w-]+$`)
)

// validAWSRegions are the regions ConvDeploy supports for environment provisioning.
var validAWSRegions = map[string]bool{
	"us-east-1": true, "us-east-2": true, "us-west-1": true, "us-west-2": true,
	"eu-west-1": true, "eu-west-2": true, "eu-west-3": true, "eu-central-1": true, "eu-north-1": true,
	"ap-south-1": true, "ap-southeast-1": true, "ap-southeast-2": true,
	"ap-northeast-1": true, "ap-northeast-2": true,
	"ca-central-1": true, "sa-east-1": true,
}

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
	if !validAWSRegions[req.AWSRegion] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported AWS region: " + req.AWSRegion})
		return
	}

	// Look up the project's account_id so we can inherit it on the environment.
	var projectAccountID *uuid.UUID
	err = s.db.Pool.QueryRow(c.Request.Context(),
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

	err = s.db.Pool.QueryRow(c.Request.Context(),
		`INSERT INTO environments (project_id, name, aws_region, account_id, stack_status)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at, updated_at`,
		env.ProjectID, env.Name, env.AWSRegion, env.AccountID, env.StackStatus,
	).Scan(&env.ID, &env.CreatedAt, &env.UpdatedAt)
	if err != nil {
		// Unique violation (23505): this project already has an environment with that name.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("a %s environment already exists for this project", req.Name)})
			return
		}
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

	rows, err := s.db.Pool.Query(c.Request.Context(),
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
	// AWS accounts belong to the active workspace; any member can list them.
	orgID, _, ok := middleware.ActiveOrg(c, s.db)
	if !ok {
		return
	}

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT a.id, a.user_id, a.org_id, a.label, a.aws_account_id, a.iam_role_arn,
		        a.last_scanned_at, a.created_at, a.updated_at,
		        COALESCE(r.cnt, 0) AS resource_count
		 FROM aws_accounts a
		 LEFT JOIN (
		     SELECT aws_account_id, COUNT(*) AS cnt FROM discovered_resources GROUP BY aws_account_id
		 ) r ON r.aws_account_id = a.id
		 WHERE a.org_id = $1 ORDER BY a.created_at ASC`, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch AWS accounts"})
		return
	}
	defer rows.Close()

	// Response embeds the account plus discovery-related UI fields.
	type accountWithDiscovery struct {
		models.AWSAccount
		ResourceCount int `json:"resource_count"`
	}
	accounts := []accountWithDiscovery{}
	for rows.Next() {
		var a accountWithDiscovery
		if err := rows.Scan(&a.ID, &a.UserID, &a.OrgID, &a.Label, &a.AWSAccountID, &a.IAMRoleARN,
			&a.LastScannedAt, &a.CreatedAt, &a.UpdatedAt, &a.ResourceCount); err != nil {
			continue
		}
		accounts = append(accounts, a)
	}
	c.JSON(http.StatusOK, accounts)
}

// HandleConnectAWSAccount saves a new AWS account record for the active workspace.
// Connecting AWS is an admin-only action.
func (s *Service) HandleConnectAWSAccount(c *gin.Context) {
	userID, ok := getUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}
	orgID, role, ok := middleware.ActiveOrg(c, s.db)
	if !ok {
		return
	}
	if role != models.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "connecting an AWS account requires the admin role in this workspace"})
		return
	}

	var req struct {
		Label        string `json:"label" binding:"required"`
		AWSAccountID string `json:"aws_account_id" binding:"required"`
		IAMRoleARN   string `json:"iam_role_arn" binding:"required"`
		ExternalID   string `json:"external_id"`
		// Optional ACM certificate ARN — enables HTTPS on platform stacks
		// provisioned in this account (set before the first environment).
		CertificateARN string `json:"certificate_arn"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !awsAccountIDRe.MatchString(req.AWSAccountID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "aws_account_id must be a 12-digit AWS account ID"})
		return
	}
	m := iamRoleARNRe.FindStringSubmatch(req.IAMRoleARN)
	if m == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "iam_role_arn must be a valid IAM role ARN (arn:aws:iam::<account>:role/<name>)"})
		return
	}
	if m[1] != req.AWSAccountID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the IAM role ARN belongs to a different AWS account than aws_account_id"})
		return
	}
	if len(req.Label) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "label too long (max 100 characters)"})
		return
	}
	var certARN *string
	if req.CertificateARN != "" {
		if !acmCertARNRe.MatchString(req.CertificateARN) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "certificate_arn must be a valid ACM certificate ARN (arn:aws:acm:<region>:<account>:certificate/<id>)"})
			return
		}
		certARN = &req.CertificateARN
	}

	// The external ID is the per-connection value embedded in the bootstrap template the
	// user just deployed (returned by HandleGetBootstrapTemplate). Older clients that don't
	// send one fall back to the legacy shared value so their pre-existing roles still work.
	externalID := req.ExternalID
	if externalID == "" {
		externalID = LegacyExternalID
	}

	// Verify the role can actually be assumed with this external ID before persisting it.
	// This also enforces that the stored external ID matches the deployed role's trust policy.
	if _, err := s.assumeRole(c.Request.Context(), req.IAMRoleARN, "us-east-1", "verify", externalID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "could not assume provided IAM role — check permissions and that you ran the latest setup script"})
		return
	}

	account := &models.AWSAccount{
		UserID:         userID,
		OrgID:          &orgID,
		Label:          req.Label,
		AWSAccountID:   req.AWSAccountID,
		IAMRoleARN:     req.IAMRoleARN,
		ExternalID:     externalID,
		CertificateARN: certARN,
	}

	err := s.db.Pool.QueryRow(c.Request.Context(),
		`INSERT INTO aws_accounts (user_id, org_id, label, aws_account_id, iam_role_arn, external_id, certificate_arn)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at, updated_at`,
		account.UserID, account.OrgID, account.Label, account.AWSAccountID, account.IAMRoleARN, account.ExternalID, account.CertificateARN,
	).Scan(&account.ID, &account.CreatedAt, &account.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save AWS account"})
		return
	}

	if s.events != nil {
		perTenant := externalID != LegacyExternalID
		s.events.EmitAccount(c.Request.Context(), account.ID, models.EventExternalIDGenerated,
			models.SeverityInfo, models.SourceDeployer,
			map[string]any{"per_tenant": perTenant, "aws_account_id": req.AWSAccountID})
	}

	// Kick off an initial discovery scan so the user immediately sees their existing
	// infrastructure (best-effort; runs async).
	if s.onAccountConnected != nil {
		s.onAccountConnected(orgID, account.ID)
	}

	c.JSON(http.StatusCreated, account)
}

// HandleDeleteAWSAccount disconnects an AWS account from the active workspace.
// Admin-only.
func (s *Service) HandleDeleteAWSAccount(c *gin.Context) {
	orgID, role, ok := middleware.ActiveOrg(c, s.db)
	if !ok {
		return
	}
	if role != models.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "disconnecting an AWS account requires the admin role in this workspace"})
		return
	}

	accountID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}

	result, err := s.db.Pool.Exec(c.Request.Context(),
		`DELETE FROM aws_accounts WHERE id = $1 AND org_id = $2`,
		accountID, orgID,
	)
	if err != nil {
		// Foreign-key violation (23503): the account is still linked to projects or
		// environments. Surface a clear 409 instead of a generic 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			c.JSON(http.StatusConflict, gin.H{"error": "This AWS account is still linked to one or more projects or environments — remove those first, then disconnect the account."})
			return
		}
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

	// Reset status to provisioning. The project_id predicate plus the RowsAffected check
	// ensures a foreign envId can never trigger provisioning of another tenant's environment.
	result, err := s.db.Pool.Exec(c.Request.Context(),
		`UPDATE environments SET stack_status = $1, updated_at = NOW() WHERE id = $2 AND project_id = $3`,
		models.StackStatusProvisioning, envID, projectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update environment"})
		return
	}
	if result.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "environment not found"})
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

	// Generate a fresh per-connection external ID and embed it in the trust policy.
	// The frontend echoes this value back on connect (HandleConnectAWSAccount) so the
	// stored external ID matches the one the user deployed into their role.
	externalID := NewExternalID()
	rendered := renderBootstrapTemplate(s.platformAccountID, externalID)
	script := renderCloudShellScript(rendered, region, s.platformAccountID, s.platformCallerARN)

	c.JSON(http.StatusOK, gin.H{
		"template":            rendered,
		"script":              script,
		"platform_account_id": s.platformAccountID,
		"region":              region,
		"external_id":         externalID,
	})
}

// renderBootstrapTemplate returns the bootstrap YAML with the ConvDeployAccountId
// parameter replaced by the platform account ID and the external-ID placeholder replaced
// by this connection's external ID, so users get a complete, ready-to-run template.
func renderBootstrapTemplate(platformAccountID, externalID string) string {
	// Replace the parameter reference and remove the Parameters block.
	rendered := strings.ReplaceAll(
		BootstrapTemplate,
		"!Sub 'arn:aws:iam::${ConvDeployAccountId}:root'",
		fmt.Sprintf("'arn:aws:iam::%s:root'", platformAccountID),
	)
	// Substitute the per-connection external ID into the trust condition and output.
	rendered = strings.ReplaceAll(rendered, externalIDPlaceholder, externalID)
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
		        https_enabled, created_at, updated_at
		 FROM platform_stacks WHERE id = $1`, id,
	).Scan(
		&ps.ID, &ps.AccountID, &ps.AWSRegion, &ps.StackID, &ps.StackStatus,
		&ps.ECSClusterName, &ps.ALBArn, &ps.ALBDNS, &ps.ALBListenerArn,
		&ps.ALBSecurityGroupID, &ps.ECSSecurityGroupID, &ps.SubnetIDs,
		&ps.HTTPSEnabled, &ps.CreatedAt, &ps.UpdatedAt,
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
		        https_enabled, created_at, updated_at
		 FROM platform_stacks WHERE account_id = $1 AND aws_region = $2`,
		accountID, region,
	).Scan(
		&ps.ID, &ps.AccountID, &ps.AWSRegion, &ps.StackID, &ps.StackStatus,
		&ps.ECSClusterName, &ps.ALBArn, &ps.ALBDNS, &ps.ALBListenerArn,
		&ps.ALBSecurityGroupID, &ps.ECSSecurityGroupID, &ps.SubnetIDs,
		&ps.HTTPSEnabled, &ps.CreatedAt, &ps.UpdatedAt,
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
		Tags:                       awstags.ToELB(awstags.BuildResourceTags(project.ID.String(), env.Name, s.platformAccountID)),
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

	ruleTags := awstags.ToELB(awstags.BuildResourceTags(env.ProjectID.String(), env.Name, s.platformAccountID))
	out, err := clients.ELB.CreateRule(ctx, &elasticloadbalancingv2.CreateRuleInput{
		Tags:        ruleTags,
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
				Tags:        ruleTags,
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
		Tags:                 awstags.ToECS(awstags.BuildResourceTags(project.ID.String(), env.Name, s.platformAccountID)),
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

// ---- Project cleanup (deletion) ----

// DeleteECSService deletes an ECS service (scaling to 0 first so tasks are drained).
// Idempotent — returns nil if the service is already missing.
func (s *Service) DeleteECSService(ctx context.Context, clients *ClientBundle, clusterName, serviceName string) error {
	// Scale to 0 so tasks stop cleanly before deletion.
	_, _ = clients.ECS.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:      aws.String(clusterName),
		Service:      aws.String(serviceName),
		DesiredCount: aws.Int32(0),
	})

	_, err := clients.ECS.DeleteService(ctx, &ecs.DeleteServiceInput{
		Cluster: aws.String(clusterName),
		Service: aws.String(serviceName),
		Force:   aws.Bool(true), // force-delete even if tasks are still running
	})
	if err != nil && !strings.Contains(err.Error(), "ServiceNotFoundException") &&
		!strings.Contains(err.Error(), "ClusterNotFoundException") {
		return fmt.Errorf("failed to delete ECS service: %w", err)
	}
	return nil
}

// DeleteListenerRule deletes an ALB listener rule. Idempotent.
func (s *Service) DeleteListenerRule(ctx context.Context, clients *ClientBundle, ruleARN string) error {
	if ruleARN == "" {
		return nil
	}
	_, err := clients.ELB.DeleteRule(ctx, &elasticloadbalancingv2.DeleteRuleInput{
		RuleArn: aws.String(ruleARN),
	})
	if err != nil && !strings.Contains(err.Error(), "RuleNotFoundException") {
		return fmt.Errorf("failed to delete ALB listener rule: %w", err)
	}
	return nil
}

// DeleteTargetGroup deletes an ALB target group. Idempotent.
func (s *Service) DeleteTargetGroup(ctx context.Context, clients *ClientBundle, tgARN string) error {
	if tgARN == "" {
		return nil
	}
	_, err := clients.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
		TargetGroupArn: aws.String(tgARN),
	})
	if err != nil && !strings.Contains(err.Error(), "TargetGroupNotFoundException") {
		return fmt.Errorf("failed to delete target group: %w", err)
	}
	return nil
}

// PurgeECRRepository deletes every image in an ECR repository so CloudFormation can
// subsequently delete the empty repo. Idempotent — returns nil if the repo is missing.
func (s *Service) PurgeECRRepository(ctx context.Context, clients *ClientBundle, repoName string) error {
	if repoName == "" {
		return nil
	}
	out, err := clients.ECR.ListImages(ctx, &ecr.ListImagesInput{
		RepositoryName: aws.String(repoName),
	})
	if err != nil {
		if strings.Contains(err.Error(), "RepositoryNotFoundException") {
			return nil
		}
		return fmt.Errorf("failed to list ECR images: %w", err)
	}
	if len(out.ImageIds) == 0 {
		return nil
	}
	_, err = clients.ECR.BatchDeleteImage(ctx, &ecr.BatchDeleteImageInput{
		RepositoryName: aws.String(repoName),
		ImageIds:       out.ImageIds,
	})
	if err != nil && !strings.Contains(err.Error(), "RepositoryNotFoundException") {
		return fmt.Errorf("failed to batch-delete ECR images: %w", err)
	}
	return nil
}

// AssumeRoleForAccount creates scoped AWS SDK clients by assuming the IAM role for the
// given account record. Used by cleanup workflows that operate without an environment.
func (s *Service) AssumeRoleForAccount(ctx context.Context, iamRoleARN, externalID, region string) (*ClientBundle, error) {
	if externalID == "" {
		externalID = LegacyExternalID
	}
	return s.assumeRole(ctx, iamRoleARN, region, "cleanup", externalID)
}

// accountCreds loads the IAM role ARN and external ID for an AWS account.
func (s *Service) accountCreds(ctx context.Context, accountID uuid.UUID) (iamRoleARN, externalID string, err error) {
	err = s.db.Pool.QueryRow(ctx,
		`SELECT iam_role_arn, external_id FROM aws_accounts WHERE id = $1`, accountID,
	).Scan(&iamRoleARN, &externalID)
	if externalID == "" {
		externalID = LegacyExternalID
	}
	return iamRoleARN, externalID, err
}

// AssumeRoleForAccountAndRegion returns a full ClientBundle for an account in a
// specific region. Used by the monitor to poll discovered ECS services that are not
// tied to an OpsPilot environment.
func (s *Service) AssumeRoleForAccountAndRegion(ctx context.Context, accountID uuid.UUID, region string) (*ClientBundle, error) {
	iamRoleARN, externalID, err := s.accountCreds(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("load AWS account %s: %w", accountID, err)
	}
	return s.assumeRole(ctx, iamRoleARN, region, "monitor", externalID)
}

// AssumeRoleConfigForAccount returns the assumed-role aws.Config for an account in a
// region, so callers (the discovery scanner) can build their own SDK clients for
// services not present in ClientBundle (RDS, ElastiCache, Lambda, S3, SQS).
func (s *Service) AssumeRoleConfigForAccount(ctx context.Context, accountID uuid.UUID, region string) (aws.Config, error) {
	iamRoleARN, externalID, err := s.accountCreds(ctx, accountID)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS account %s: %w", accountID, err)
	}
	return s.assumeRoleConfig(ctx, iamRoleARN, region, "discovery", externalID)
}

// assumeRoleConfig mirrors assumeRole but returns the raw assumed-role config instead
// of a ClientBundle. It verifies the role can be assumed up front (eager Retrieve).
func (s *Service) assumeRoleConfig(ctx context.Context, iamRoleARN, region, sessionSuffix, externalID string) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return aws.Config{}, fmt.Errorf("failed to load AWS config: %w", err)
	}
	provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), iamRoleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = fmt.Sprintf("convdeploy-%s", sessionSuffix)
		o.ExternalID = aws.String(externalID)
	})
	if _, err := provider.Retrieve(ctx); err != nil {
		return aws.Config{}, s.explainAssumeRoleError(err, iamRoleARN)
	}
	return config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(aws.NewCredentialsCache(provider)),
	)
}

// AccountRegions returns the distinct AWS regions an account is used in (its
// environments and platform stacks), defaulting to us-east-1 when the account has no
// resources yet (e.g. freshly connected). This bounds the discovery scan to regions
// the customer actually uses rather than all ~30 AWS regions.
func (s *Service) AccountRegions(ctx context.Context, accountID uuid.UUID) ([]string, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT DISTINCT region FROM (
			SELECT e.aws_region AS region FROM environments e WHERE e.account_id = $1
			UNION
			SELECT ps.aws_region AS region FROM platform_stacks ps WHERE ps.account_id = $1
		) r WHERE region IS NOT NULL AND region <> ''`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var regions []string
	for rows.Next() {
		var r string
		if rows.Scan(&r) == nil && r != "" {
			regions = append(regions, r)
		}
	}
	if len(regions) == 0 {
		regions = []string{"us-east-1"}
	}
	return regions, rows.Err()
}

// MarkAccountScanned records that the discovery scanner just completed for an account.
func (s *Service) MarkAccountScanned(ctx context.Context, accountID uuid.UUID) {
	_, _ = s.db.Pool.Exec(ctx, `UPDATE aws_accounts SET last_scanned_at = NOW() WHERE id = $1`, accountID)
}

// SetOnAccountConnected registers a callback invoked after an AWS account is
// successfully connected — used to enqueue an initial discovery scan. Non-blocking.
func (s *Service) SetOnAccountConnected(fn func(orgID, accountID uuid.UUID)) {
	s.onAccountConnected = fn
}

// ServiceHealth is a point-in-time snapshot of an ECS service's runtime state.
type ServiceHealth struct {
	RunningCount int32
	DesiredCount int32
	PendingCount int32
	RolloutState string // COMPLETED | IN_PROGRESS | FAILED | "" (no primary deployment)
	Reason       string // rollout state reason, when present
}

// DescribeECSService returns the current running/desired counts and rollout state for a service.
// Used by the health-check workflow. Returns an error if the service does not exist.
func (s *Service) DescribeECSService(ctx context.Context, clients *ClientBundle, clusterName, serviceName string) (*ServiceHealth, error) {
	out, err := clients.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []string{serviceName},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe ECS service: %w", err)
	}
	if len(out.Services) == 0 || (out.Services[0].Status != nil && *out.Services[0].Status == "INACTIVE") {
		return nil, fmt.Errorf("service %q not found or inactive — deploy first", serviceName)
	}

	svc := out.Services[0]
	h := &ServiceHealth{
		RunningCount: svc.RunningCount,
		DesiredCount: svc.DesiredCount,
		PendingCount: svc.PendingCount,
	}
	for _, d := range svc.Deployments {
		if d.Status != nil && *d.Status == "PRIMARY" {
			h.RolloutState = string(d.RolloutState)
			h.Reason = aws.ToString(d.RolloutStateReason)
			break
		}
	}
	return h, nil
}

// UpdateServiceDesiredCount changes the desired task count of an existing ECS service.
// Used by the scale workflow. The service must already exist (created at first deploy).
func (s *Service) UpdateServiceDesiredCount(ctx context.Context, clients *ClientBundle, clusterName, serviceName string, desiredCount int32) error {
	_, err := clients.ECS.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:      aws.String(clusterName),
		Service:      aws.String(serviceName),
		DesiredCount: aws.Int32(desiredCount),
	})
	if err != nil {
		return fmt.Errorf("failed to update desired count: %w", err)
	}
	return nil
}

// GetRunningTask returns the ARN of the first running task for a given ECS service.
// Used by the terminal proxy to find a container to exec into.
func (s *Service) GetRunningTask(ctx context.Context, clients *ClientBundle, clusterName, serviceName string) (string, error) {
	out, err := clients.ECS.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster:       aws.String(clusterName),
		ServiceName:   aws.String(serviceName),
		DesiredStatus: ecstypes.DesiredStatusRunning,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list tasks: %w", err)
	}
	if len(out.TaskArns) == 0 {
		return "", fmt.Errorf("no running tasks found for service %q — is the service deployed?", serviceName)
	}
	return out.TaskArns[0], nil
}

// StartExecSession starts an ECS Exec session for interactive shell access.
// Returns the SSM StreamUrl and TokenValue needed to open the datachannel.
func (s *Service) StartExecSession(ctx context.Context, clients *ClientBundle, clusterName, taskARN, command string) (streamURL, tokenValue string, err error) {
	out, err := clients.ECS.ExecuteCommand(ctx, &ecs.ExecuteCommandInput{
		Cluster:     aws.String(clusterName),
		Task:        aws.String(taskARN),
		Container:   aws.String("app"),
		Command:     aws.String(command),
		Interactive: true,
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to start ECS exec session: %w", err)
	}
	if out.Session == nil {
		return "", "", fmt.Errorf("ECS exec returned empty session")
	}
	return aws.ToString(out.Session.StreamUrl), aws.ToString(out.Session.TokenValue), nil
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
	// Python
	case "fastapi":
		return "uvicorn main:app --host 0.0.0.0 --port 8000"
	case "flask":
		return "gunicorn main:app -b 0.0.0.0:8000"
	case "django":
		return "gunicorn config.wsgi:application -b 0.0.0.0:8000"
	case "python":
		return "python main.py"
	// Node.js / JS
	case "nodejs", "express":
		return "node index.js"
	case "nextjs", "remix":
		return "node server.js"
	case "nestjs":
		return "node dist/main.js"
	case "nuxtjs":
		return "node .output/server/index.mjs"
	case "svelte":
		return "node build/index.js"
	case "astro":
		return "node ./dist/server/entry.mjs"
	// Go
	case "go":
		return "./server"
	// Ruby
	case "rails":
		return "bundle exec puma -C config/puma.rb"
	// Java
	case "spring":
		return "java -jar app.jar"
	// Static
	case "static":
		return "nginx -g 'daemon off;'"
	case "react-spa", "vite":
		return "nginx -g 'daemon off;'"
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
	case "fastapi", "flask", "django":
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

	case "nodejs", "express":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/node:20-alpine
WORKDIR /app
COPY package*.json ./
RUN [ -f package-lock.json ] && npm ci --only=production || npm install --only=production
COPY . .
EXPOSE %d
%s`, port, cmd)

	case "nestjs":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/node:20-alpine AS builder
WORKDIR /app
COPY package*.json ./
RUN [ -f package-lock.json ] && npm ci || npm install
COPY . .
RUN npm run build

FROM public.ecr.aws/docker/library/node:20-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/node_modules ./node_modules
EXPOSE %d
%s`, port, cmd)

	case "nextjs":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/node:20-alpine AS deps
WORKDIR /app
COPY package*.json ./
RUN [ -f package-lock.json ] && npm ci || npm install

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

	case "remix", "nuxtjs", "svelte", "astro":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/node:20-alpine AS builder
WORKDIR /app
COPY package*.json ./
RUN [ -f package-lock.json ] && npm ci || npm install
COPY . .
RUN npm run build

FROM public.ecr.aws/docker/library/node:20-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
COPY --from=builder /app/build ./build
COPY --from=builder /app/.output ./.output
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/node_modules ./node_modules
EXPOSE %d
%s`, port, cmd)

	case "go":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o server .

FROM public.ecr.aws/docker/library/alpine:3.19
WORKDIR /app
COPY --from=builder /app/server .
EXPOSE %d
%s`, port, cmd)

	case "rails":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/ruby:3.2-slim
RUN apt-get update -qq && apt-get install -y build-essential libpq-dev nodejs && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY Gemfile Gemfile.lock ./
RUN bundle install --without development test
COPY . .
RUN SECRET_KEY_BASE=dummy bundle exec rake assets:precompile 2>/dev/null || true
EXPOSE %d
%s`, port, cmd)

	case "spring":
		return fmt.Sprintf(`FROM public.ecr.aws/docker/library/eclipse-temurin:21-jdk-alpine AS builder
WORKDIR /app
COPY . .
RUN chmod +x mvnw 2>/dev/null; chmod +x gradlew 2>/dev/null; true
RUN if [ -f mvnw ]; then ./mvnw package -DskipTests -q; \
    elif [ -f pom.xml ]; then mvn package -DskipTests -q; \
    elif [ -f gradlew ]; then ./gradlew bootJar -x test -q; \
    else gradle bootJar -x test -q; fi && \
    find . -name "*.jar" -not -name "*sources*" -not -name "*javadoc*" | head -1 | xargs -I{} cp {} /app.jar

FROM public.ecr.aws/docker/library/eclipse-temurin:21-jre-alpine
WORKDIR /app
COPY --from=builder /app.jar app.jar
EXPOSE %d
%s`, port, cmd)

	case "static":
		return fmt.Sprintf(`FROM public.ecr.aws/nginx/nginx:stable-alpine
COPY . /usr/share/nginx/html
EXPOSE %d
CMD ["nginx", "-g", "daemon off;"]`, port)

	case "react-spa":
		return `FROM public.ecr.aws/docker/library/node:20-alpine AS builder
WORKDIR /app
COPY package*.json ./
RUN [ -f package-lock.json ] && npm ci || npm install
COPY . .
RUN npm run build

FROM public.ecr.aws/nginx/nginx:stable-alpine
COPY --from=builder /app/build /usr/share/nginx/html
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]`

	case "vite":
		return `FROM public.ecr.aws/docker/library/node:20-alpine AS builder
WORKDIR /app
COPY package*.json ./
RUN [ -f package-lock.json ] && npm ci || npm install
COPY . .
RUN npm run build

FROM public.ecr.aws/nginx/nginx:stable-alpine
COPY --from=builder /app/dist /usr/share/nginx/html
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]`

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
	case "nodejs", "express", "nestjs", "nextjs", "remix", "nuxtjs", "svelte", "astro", "rails":
		return 3000
	case "go", "spring":
		return 8080
	case "static":
		return 80
	case "react-spa", "vite":
		return 80
	default:
		return 8000
	}
}

// taskResources returns Fargate CPU and memory for an environment tier.
// Staging:    512 vCPU units / 1024 MB — handles JVM cold-start (Spring Boot, Rails)
// Production: 1024 vCPU units / 2048 MB — headroom for real traffic
func taskResources(envName string) (cpu, memory string) {
	if envName == "production" {
		return "1024", "2048"
	}
	return "512", "1024"
}

// ---- CloudWatch helpers ----

// CreateLogGroupIfNotExists creates a CloudWatch Logs log group.
// Ignores ResourceAlreadyExistsException so it can be called idempotently.
func (s *Service) CreateLogGroupIfNotExists(ctx context.Context, clients *ClientBundle, logGroupName string) {
	_, err := clients.CloudWatch.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(logGroupName),
	})
	if err != nil {
		// ResourceAlreadyExistsException is expected — ignore it.
		if !strings.Contains(err.Error(), "ResourceAlreadyExistsException") {
			slog.Error(fmt.Sprintf("[aws] failed to create log group %s: %v", logGroupName, err))
		}
	}
}

// ---- Cost Intelligence ----

// GetAccountCostSummary fetches the last 30 days of AWS costs for the account linked to the
// given project. Only the services ConvDeploy provisions are included in the query.
func (s *Service) GetAccountCostSummary(ctx context.Context, projectID string) (*models.CostSummary, error) {
	var iamRoleARN, externalID string
	err := s.db.Pool.QueryRow(ctx, `
		SELECT a.iam_role_arn, a.external_id
		FROM aws_accounts a
		JOIN projects p ON p.account_id = a.id
		WHERE p.id = $1
	`, projectID).Scan(&iamRoleARN, &externalID)
	if err != nil {
		return nil, fmt.Errorf("no AWS account linked: %w", err)
	}
	if externalID == "" {
		externalID = LegacyExternalID
	}

	clients, err := s.assumeRole(ctx, iamRoleARN, "us-east-1", projectID, externalID)
	if err != nil {
		return nil, fmt.Errorf("failed to assume role: %w", err)
	}

	end := time.Now().UTC()
	start := end.AddDate(0, 0, -30)
	startStr := start.Format("2006-01-02")
	endStr := end.Format("2006-01-02")

	out, err := clients.CostExplorer.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
		TimePeriod:  &cetypes.DateInterval{Start: aws.String(startStr), End: aws.String(endStr)},
		Granularity: cetypes.GranularityMonthly,
		Filter: &cetypes.Expression{
			Dimensions: &cetypes.DimensionValues{
				Key: cetypes.DimensionService,
				Values: []string{
					"Amazon Elastic Container Service",
					"Amazon EC2 Container Registry (ECR)",
					"AWS CodeBuild",
					"Amazon Elastic Load Balancing",
					"Amazon Virtual Private Cloud",
				},
			},
		},
		GroupBy: []cetypes.GroupDefinition{
			{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("SERVICE")},
		},
		Metrics: []string{"UnblendedCost"},
	})
	if err != nil {
		return nil, fmt.Errorf("cost explorer query failed: %w", err)
	}

	summary := &models.CostSummary{
		ByService:   make(map[string]float64),
		Currency:    "USD",
		PeriodStart: startStr,
		PeriodEnd:   endStr,
	}

	for _, result := range out.ResultsByTime {
		for _, group := range result.Groups {
			if len(group.Keys) == 0 {
				continue
			}
			name := group.Keys[0]
			if metric, ok := group.Metrics["UnblendedCost"]; ok {
				val, _ := strconv.ParseFloat(aws.ToString(metric.Amount), 64)
				summary.ByService[name] += val
				summary.TotalMonthlyCost += val
				if metric.Unit != nil {
					summary.Currency = *metric.Unit
				}
			}
		}
	}

	return summary, nil
}

// GetCostTotalForRange returns the total unblended platform cost for a project's AWS
// account over [startStr, endStr) (dates as YYYY-MM-DD), using daily granularity. Used by
// the daily summary's cost-change comparison (7d vs prior 7d).
func (s *Service) GetCostTotalForRange(ctx context.Context, projectID, startStr, endStr string) (float64, error) {
	var iamRoleARN, externalID string
	err := s.db.Pool.QueryRow(ctx, `
		SELECT a.iam_role_arn, a.external_id
		FROM aws_accounts a JOIN projects p ON p.account_id = a.id
		WHERE p.id = $1`, projectID).Scan(&iamRoleARN, &externalID)
	if err != nil {
		return 0, fmt.Errorf("no AWS account linked: %w", err)
	}
	if externalID == "" {
		externalID = LegacyExternalID
	}
	clients, err := s.assumeRole(ctx, iamRoleARN, "us-east-1", projectID, externalID)
	if err != nil {
		return 0, fmt.Errorf("failed to assume role: %w", err)
	}

	out, err := clients.CostExplorer.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
		TimePeriod:  &cetypes.DateInterval{Start: aws.String(startStr), End: aws.String(endStr)},
		Granularity: cetypes.GranularityDaily,
		Filter: &cetypes.Expression{
			Dimensions: &cetypes.DimensionValues{
				Key: cetypes.DimensionService,
				Values: []string{
					"Amazon Elastic Container Service",
					"Amazon EC2 Container Registry (ECR)",
					"AWS CodeBuild",
					"Amazon Elastic Load Balancing",
					"Amazon Virtual Private Cloud",
				},
			},
		},
		Metrics: []string{"UnblendedCost"},
	})
	if err != nil {
		return 0, fmt.Errorf("cost explorer query failed: %w", err)
	}

	var total float64
	for _, result := range out.ResultsByTime {
		if metric, ok := result.Total["UnblendedCost"]; ok {
			val, _ := strconv.ParseFloat(aws.ToString(metric.Amount), 64)
			total += val
		}
	}
	return total, nil
}

// ---- Conversational Infrastructure Mutations ----

// GetCurrentTaskResources returns the CPU and memory of the currently running task definition
// for the given ECS service.
func (s *Service) GetCurrentTaskResources(ctx context.Context, clients *ClientBundle, clusterName, serviceName string) (cpu, memory string, err error) {
	out, err := clients.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []string{serviceName},
	})
	if err != nil || len(out.Services) == 0 {
		return "", "", fmt.Errorf("service not found")
	}
	taskDefARN := aws.ToString(out.Services[0].TaskDefinition)

	td, err := clients.ECS.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefARN),
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to describe task definition: %w", err)
	}
	return aws.ToString(td.TaskDefinition.Cpu), aws.ToString(td.TaskDefinition.Memory), nil
}

// UpdateServiceResources registers a new task definition revision with the given CPU/memory
// and forces a service update. The new tasks will roll out gradually.
func (s *Service) UpdateServiceResources(ctx context.Context, clients *ClientBundle, env *models.Environment, project *models.Project, imageURI, cpu, memory string) error {
	if env.TaskExecutionRoleARN == nil || env.LogGroupName == nil {
		return fmt.Errorf("environment not fully provisioned")
	}

	port := frameworkPort(project.Framework)
	family := fmt.Sprintf("convdeploy-%s", project.ID.String())

	var envVars []ecstypes.KeyValuePair
	rows, _ := s.db.Pool.Query(ctx,
		`SELECT key, value FROM env_vars WHERE environment_id = $1`, env.ID)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if rows.Scan(&k, &v) == nil {
				envVars = append(envVars, ecstypes.KeyValuePair{Name: aws.String(k), Value: aws.String(v)})
			}
		}
	}

	reg, err := clients.ECS.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Tags:                    awstags.ToECS(awstags.BuildResourceTags(project.ID.String(), env.Name, s.platformAccountID)),
		Family:                  aws.String(family),
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		Cpu:                     aws.String(cpu),
		Memory:                  aws.String(memory),
		ExecutionRoleArn:        env.TaskExecutionRoleARN,
		TaskRoleArn:             env.TaskExecutionRoleARN,
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:        aws.String("app"),
				Image:       aws.String(imageURI),
				Essential:   aws.Bool(true),
				Environment: envVars,
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
	})
	if err != nil {
		return fmt.Errorf("failed to register task definition: %w", err)
	}

	clusterName, _, _, _, err := s.resolveNetworkingForEnv(ctx, env)
	if err != nil {
		return err
	}

	_, err = clients.ECS.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:            aws.String(clusterName),
		Service:            env.ECSServiceName,
		TaskDefinition:     reg.TaskDefinition.TaskDefinitionArn,
		ForceNewDeployment: true,
	})
	return err
}

// resolveNetworkingForEnv is a helper to get the cluster name from platform stack or legacy fields.
func (s *Service) resolveNetworkingForEnv(ctx context.Context, env *models.Environment) (clusterName string, subnets []string, sgID string, listenerARN string, err error) {
	if env.PlatformStackID != nil {
		ps, psErr := s.GetPlatformStack(ctx, *env.PlatformStackID)
		if psErr != nil {
			return "", nil, "", "", psErr
		}
		if ps.ECSClusterName == nil {
			return "", nil, "", "", fmt.Errorf("platform stack not fully provisioned")
		}
		la := ""
		if ps.ALBListenerArn != nil {
			la = *ps.ALBListenerArn
		}
		var subs []string
		if ps.SubnetIDs != nil {
			subs = strings.Split(*ps.SubnetIDs, ",")
		}
		sg := ""
		if ps.ECSSecurityGroupID != nil {
			sg = *ps.ECSSecurityGroupID
		}
		return *ps.ECSClusterName, subs, sg, la, nil
	}
	if env.ECSClusterName == nil {
		return "", nil, "", "", fmt.Errorf("cluster name not set")
	}
	subs := []string{}
	if env.VPCSubnets != nil {
		subs = strings.Split(*env.VPCSubnets, ",")
	}
	sg := ""
	if env.ECSSecurityGroupID != nil {
		sg = *env.ECSSecurityGroupID
	}
	return *env.ECSClusterName, subs, sg, "", nil
}

// ---- PR Preview Environments ----

// CreatePreviewService provisions a target group, listener rule (path /pr-N/*), and ECS
// service for a PR preview. All infra is shared from the staging platform stack.
func (s *Service) CreatePreviewService(
	ctx context.Context,
	clients *ClientBundle,
	stagingEnv *models.Environment,
	ps *models.PlatformStack,
	previewEnv *models.Environment,
	imageURI string,
	project *models.Project,
) (listenerRuleARN, targetGroupARN string, err error) {
	if ps.ALBListenerArn == nil || ps.ECSClusterName == nil || ps.ECSSecurityGroupID == nil || ps.SubnetIDs == nil {
		return "", "", fmt.Errorf("platform stack not fully provisioned")
	}
	if previewEnv.PRNumber == nil {
		return "", "", fmt.Errorf("preview env missing pr_number")
	}
	if previewEnv.TaskExecutionRoleARN == nil || previewEnv.LogGroupName == nil {
		return "", "", fmt.Errorf("preview env missing execution role or log group")
	}

	prNum := *previewEnv.PRNumber
	port := frameworkPort(project.Framework)
	shortID := project.ID.String()[:8]

	// Create target group.
	tgName := fmt.Sprintf("pr-%d-%s", prNum, shortID)
	if len(tgName) > 32 {
		tgName = tgName[:32]
	}

	// Derive VPC ID from the staging ALB.
	albOut, err := clients.ELB.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []string{aws.ToString(ps.ALBArn)},
	})
	if err != nil || len(albOut.LoadBalancers) == 0 {
		return "", "", fmt.Errorf("failed to describe ALB: %w", err)
	}
	vpcID := aws.ToString(albOut.LoadBalancers[0].VpcId)

	previewTags := awstags.BuildResourceTags(project.ID.String(), previewEnv.Name, s.platformAccountID)
	tg, err := clients.ELB.CreateTargetGroup(ctx, &elasticloadbalancingv2.CreateTargetGroupInput{
		Tags:                       awstags.ToELB(previewTags),
		Name:                       aws.String(tgName),
		Protocol:                   elbtypes.ProtocolEnumHttp,
		Port:                       aws.Int32(port),
		VpcId:                      aws.String(vpcID),
		TargetType:                 elbtypes.TargetTypeEnumIp,
		HealthCheckPath:            aws.String("/"),
		HealthCheckIntervalSeconds: aws.Int32(30),
		HealthyThresholdCount:      aws.Int32(2),
		UnhealthyThresholdCount:    aws.Int32(3),
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to create target group: %w", err)
	}
	tgARN := aws.ToString(tg.TargetGroups[0].TargetGroupArn)

	// Create listener rule at priority 10000 + prNum with path /pr-N/*.
	priority := int32(10000 + prNum)
	rule, err := clients.ELB.CreateRule(ctx, &elasticloadbalancingv2.CreateRuleInput{
		Tags:        awstags.ToELB(previewTags),
		ListenerArn: ps.ALBListenerArn,
		Priority:    aws.Int32(priority),
		Conditions: []elbtypes.RuleCondition{
			{
				Field:  aws.String("path-pattern"),
				Values: []string{fmt.Sprintf("/pr-%d/*", prNum)},
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
		// Clean up TG on failure.
		clients.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(tgARN)})
		return "", "", fmt.Errorf("failed to create listener rule: %w", err)
	}
	ruleARN := aws.ToString(rule.Rules[0].RuleArn)

	// Create ECS service.
	subnets := strings.Split(*ps.SubnetIDs, ",")
	serviceName := fmt.Sprintf("%s-pr-%d", project.ID.String()[:8], prNum)

	envVars := []ecstypes.KeyValuePair{
		{Name: aws.String("PREVIEW_PATH_PREFIX"), Value: aws.String(fmt.Sprintf("/pr-%d", prNum))},
	}
	rows, _ := s.db.Pool.Query(ctx, `SELECT key, value FROM env_vars WHERE environment_id = $1`, stagingEnv.ID)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if rows.Scan(&k, &v) == nil {
				envVars = append(envVars, ecstypes.KeyValuePair{Name: aws.String(k), Value: aws.String(v)})
			}
		}
	}

	family := fmt.Sprintf("convdeploy-pr-%d-%s", prNum, project.ID.String()[:8])
	reg, err := clients.ECS.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Tags:                    awstags.ToECS(previewTags),
		Family:                  aws.String(family),
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		Cpu:                     aws.String("512"),
		Memory:                  aws.String("1024"),
		ExecutionRoleArn:        previewEnv.TaskExecutionRoleARN,
		TaskRoleArn:             previewEnv.TaskExecutionRoleARN,
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:        aws.String("app"),
				Image:       aws.String(imageURI),
				Essential:   aws.Bool(true),
				Environment: envVars,
				PortMappings: []ecstypes.PortMapping{
					{ContainerPort: aws.Int32(port), Protocol: ecstypes.TransportProtocolTcp},
				},
				LogConfiguration: &ecstypes.LogConfiguration{
					LogDriver: ecstypes.LogDriverAwslogs,
					Options: map[string]string{
						"awslogs-group":         *previewEnv.LogGroupName,
						"awslogs-region":        clients.Region,
						"awslogs-stream-prefix": "ecs-pr",
					},
				},
			},
		},
	})
	if err != nil {
		clients.ELB.DeleteRule(ctx, &elasticloadbalancingv2.DeleteRuleInput{RuleArn: aws.String(ruleARN)})
		clients.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(tgARN)})
		return "", "", fmt.Errorf("failed to register preview task definition: %w", err)
	}

	_, err = clients.ECS.CreateService(ctx, &ecs.CreateServiceInput{
		Tags:           awstags.ToECS(previewTags),
		Cluster:        aws.String(*ps.ECSClusterName),
		ServiceName:    aws.String(serviceName),
		TaskDefinition: reg.TaskDefinition.TaskDefinitionArn,
		DesiredCount:   aws.Int32(1),
		LaunchType:     ecstypes.LaunchTypeFargate,
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        subnets,
				SecurityGroups: []string{*ps.ECSSecurityGroupID},
				AssignPublicIp: ecstypes.AssignPublicIpEnabled,
			},
		},
		LoadBalancers: []ecstypes.LoadBalancer{
			{
				TargetGroupArn: aws.String(tgARN),
				ContainerName:  aws.String("app"),
				ContainerPort:  aws.Int32(port),
			},
		},
		EnableExecuteCommand: true,
	})
	if err != nil {
		clients.ELB.DeleteRule(ctx, &elasticloadbalancingv2.DeleteRuleInput{RuleArn: aws.String(ruleARN)})
		clients.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(tgARN)})
		return "", "", fmt.Errorf("failed to create preview ECS service: %w", err)
	}

	// Store service name on the preview env record.
	s.db.Pool.Exec(ctx,
		`UPDATE environments SET ecs_service_name = $1, updated_at = NOW() WHERE id = $2`,
		serviceName, previewEnv.ID,
	)

	return ruleARN, tgARN, nil
}

// TeardownPreviewService removes the ECS service, listener rule, and target group for a PR.
func (s *Service) TeardownPreviewService(ctx context.Context, clients *ClientBundle, previewEnv *models.Environment) error {
	clusterName, _, _, _, _ := s.resolveNetworkingForEnv(ctx, previewEnv)

	// Scale down to 0 first so tasks drain, then delete.
	if previewEnv.ECSServiceName != nil && clusterName != "" {
		clients.ECS.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster:      aws.String(clusterName),
			Service:      previewEnv.ECSServiceName,
			DesiredCount: aws.Int32(0),
		})
		// Brief wait for drain.
		time.Sleep(5 * time.Second)
		clients.ECS.DeleteService(ctx, &ecs.DeleteServiceInput{
			Cluster: aws.String(clusterName),
			Service: previewEnv.ECSServiceName,
			Force:   aws.Bool(true),
		})
	}

	if previewEnv.ALBListenerRuleARN != nil {
		clients.ELB.DeleteRule(ctx, &elasticloadbalancingv2.DeleteRuleInput{
			RuleArn: previewEnv.ALBListenerRuleARN,
		})
	}

	if previewEnv.ALBTargetGroupARN != nil {
		// Retry a couple of times — the TG may still have registered targets draining.
		for i := 0; i < 3; i++ {
			_, err := clients.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
				TargetGroupArn: previewEnv.ALBTargetGroupARN,
			})
			if err == nil {
				break
			}
			time.Sleep(5 * time.Second)
		}
	}

	return nil
}
