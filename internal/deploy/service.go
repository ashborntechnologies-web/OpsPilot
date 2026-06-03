package deploy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	awssvc "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/envvars"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	githubsvc "github.com/ashborntechnologies-web/OpsPilot/internal/github"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// DeleteEnvCleanupInfo carries the AWS resource references needed to tear down one
// environment. Snapshot before the DB record is deleted so the job can run afterwards.
type DeleteEnvCleanupInfo struct {
	Region             string `json:"region"`
	IAMRoleARN         string `json:"iam_role_arn"`
	ExternalID         string `json:"external_id"`
	ProjectStackID     string `json:"project_stack_id"`
	ECRRepoName        string `json:"ecr_repo_name"`
	ECSClusterName     string `json:"ecs_cluster_name"`
	ECSServiceName     string `json:"ecs_service_name"`
	ALBListenerRuleARN string `json:"alb_listener_rule_arn"`
	ALBTargetGroupARN  string `json:"alb_target_group_arn"`
}

// DeleteProjectPayload is the queue-job payload for background AWS cleanup after a project
// is deleted from the database.
type DeleteProjectPayload struct {
	ProjectName  string                 `json:"project_name"`
	Environments []DeleteEnvCleanupInfo `json:"environments"`
}

// Enqueuer is implemented by queue.Client. Defined here to avoid a circular import
// (the queue package imports this package for the worker).
type Enqueuer interface {
	EnqueueDeploy(projectID, environmentID, deploymentID, commitSHA string) error
	EnqueueProvision(projectID, environmentID string) error
	EnqueueRollback(projectID, environmentID, deploymentID, previousDeploymentID string) error
	EnqueueDeleteProject(p DeleteProjectPayload) error
}

type Service struct {
	db        *models.DB
	awsSvc    *awssvc.Service
	githubSvc *githubsvc.Service
	hub       *ws.Hub
	enqueuer  Enqueuer
	events    *events.Service
	envVars   *envvars.Service
}

func NewService(db *models.DB, awsSvc *awssvc.Service, githubSvc *githubsvc.Service, hub *ws.Hub, enqueuer Enqueuer, eventSvc *events.Service, envVarSvc *envvars.Service) *Service {
	return &Service{db: db, awsSvc: awsSvc, githubSvc: githubSvc, hub: hub, enqueuer: enqueuer, events: eventSvc, envVars: envVarSvc}
}

// ---- HTTP handlers ----

func (s *Service) HandleCreateProject(c *gin.Context) {
	var req struct {
		Name         string  `json:"name" binding:"required"`
		RepoURL      string  `json:"repo_url" binding:"required"`
		RepoOwner    string  `json:"repo_owner" binding:"required"`
		RepoName     string  `json:"repo_name" binding:"required"`
		Framework    string  `json:"framework"`
		Branch       *string `json:"branch"`
		StartCommand *string `json:"start_command"`
		AccountID    *string `json:"account_id"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	var accountID *uuid.UUID
	if req.AccountID != nil && *req.AccountID != "" {
		parsed, err := uuid.Parse(*req.AccountID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account_id"})
			return
		}
		accountID = &parsed
	}

	project := &models.Project{
		UserID:       userID,
		Name:         req.Name,
		RepoURL:      req.RepoURL,
		RepoOwner:    req.RepoOwner,
		RepoName:     req.RepoName,
		Framework:    req.Framework,
		Branch:       req.Branch,
		StartCommand: req.StartCommand,
		AccountID:    accountID,
	}

	err := s.db.Pool.QueryRow(c.Request.Context(),
		`INSERT INTO projects (user_id, name, repo_url, repo_owner, repo_name, framework, branch, start_command, account_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, created_at, updated_at`,
		project.UserID, project.Name, project.RepoURL, project.RepoOwner,
		project.RepoName, project.Framework, project.Branch, project.StartCommand, project.AccountID,
	).Scan(&project.ID, &project.CreatedAt, &project.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create project"})
		return
	}

	c.JSON(http.StatusCreated, project)
}

func (s *Service) HandleListProjects(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT id, user_id, name, repo_url, repo_owner, repo_name, framework, branch, start_command, account_id, created_at, updated_at
		 FROM projects WHERE user_id = $1 ORDER BY created_at DESC`, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch projects"})
		return
	}
	defer rows.Close()

	var projects []models.Project
	for rows.Next() {
		var p models.Project
		if err := rows.Scan(
			&p.ID, &p.UserID, &p.Name, &p.RepoURL, &p.RepoOwner,
			&p.RepoName, &p.Framework, &p.Branch, &p.StartCommand, &p.AccountID, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			continue
		}
		projects = append(projects, p)
	}

	if projects == nil {
		projects = []models.Project{}
	}
	c.JSON(http.StatusOK, projects)
}

func (s *Service) HandleGetProject(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	project, err := s.getProject(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	c.JSON(http.StatusOK, project)
}

func (s *Service) HandleDeploy(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	envName := "production"
	if e := c.Query("env"); e != "" {
		envName = e
	}

	response, err := s.TriggerDeploy(c.Request.Context(), projectID, envName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"message": response})
}

func (s *Service) HandleListDeployments(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT id, project_id, environment_id, commit_sha, commit_message, image_uri, status, failure_reason, created_at, updated_at
		 FROM deployments WHERE project_id = $1 ORDER BY created_at DESC LIMIT 20`, projectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch deployments"})
		return
	}
	defer rows.Close()

	var deployments []models.Deployment
	for rows.Next() {
		var d models.Deployment
		if err := rows.Scan(
			&d.ID, &d.ProjectID, &d.EnvironmentID, &d.CommitSHA,
			&d.CommitMessage, &d.ImageURI, &d.Status, &d.FailureReason,
			&d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			continue
		}
		deployments = append(deployments, d)
	}

	if deployments == nil {
		deployments = []models.Deployment{}
	}
	c.JSON(http.StatusOK, deployments)
}

func (s *Service) HandleRollback(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	deploymentID, err := uuid.Parse(c.Param("deployId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deployment id"})
		return
	}

	// Resolve which environment to roll back from the referenced deployment.
	dep, err := s.getDeployment(c.Request.Context(), deploymentID)
	if err != nil || dep.ProjectID != projectID {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment not found"})
		return
	}
	env, err := s.getEnvironmentByID(c.Request.Context(), dep.EnvironmentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "environment not found"})
		return
	}

	response, err := s.TriggerRollback(c.Request.Context(), projectID, env.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"message": response})
}

// HandleRedeploy re-runs an existing deployment using its original commit SHA.
func (s *Service) HandleRedeploy(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	deploymentID, err := uuid.Parse(c.Param("deployId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deployment id"})
		return
	}

	dep, err := s.getDeployment(c.Request.Context(), deploymentID)
	if err != nil || dep.ProjectID != projectID {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment not found"})
		return
	}

	commitMsg := ""
	if dep.CommitMessage != nil {
		commitMsg = *dep.CommitMessage
	}

	newDep, err := s.createDeployment(c.Request.Context(), projectID, dep.EnvironmentID, dep.CommitSHA, commitMsg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create deployment record"})
		return
	}

	if err := s.enqueuer.EnqueueDeploy(
		projectID.String(),
		dep.EnvironmentID.String(),
		newDep.ID.String(),
		dep.CommitSHA,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue redeploy job"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message":    fmt.Sprintf("Redeployment started for commit `%s`", dep.CommitSHA[:8]),
		"deployment": newDep,
	})
}

// HandleDeleteDeployment removes a deployment record and its associated operational events.
// In-progress deployments (building/deploying) cannot be deleted.
func (s *Service) HandleDeleteDeployment(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	deploymentID, err := uuid.Parse(c.Param("deployId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deployment id"})
		return
	}

	dep, err := s.getDeployment(c.Request.Context(), deploymentID)
	if err != nil || dep.ProjectID != projectID {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment not found"})
		return
	}

	if dep.Status == models.DeployStatusBuilding || dep.Status == models.DeployStatusDeploying {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot delete a deployment that is currently in progress"})
		return
	}

	// Delete operational events first to satisfy the FK constraint.
	s.db.Pool.Exec(c.Request.Context(),
		`DELETE FROM operational_events WHERE deployment_id = $1`, deploymentID)

	if _, err = s.db.Pool.Exec(c.Request.Context(),
		`DELETE FROM deployments WHERE id = $1 AND project_id = $2`, deploymentID, projectID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete deployment"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Deployment deleted"})
}

// ---- workflow methods called by conversation engine and queue workers ----

// TriggerDeploy fetches the latest commit, creates a pending deployment record,
// and enqueues a background job. Returns immediately with a status message.
func (s *Service) TriggerDeploy(ctx context.Context, projectID uuid.UUID, envName string) (string, error) {
	project, err := s.getProject(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("project not found: %w", err)
	}

	env, err := s.getEnvironment(ctx, projectID, envName)
	if err != nil {
		return "", fmt.Errorf("environment %q not found — create it first: %w", envName, err)
	}

	token, err := s.githubSvc.GetTokenForDeployment(ctx, project.UserID)
	if err != nil {
		return "", fmt.Errorf("GitHub not connected: %w", err)
	}

	branch := ""
	if project.Branch != nil {
		branch = *project.Branch
	}
	commitSHA, commitMsg, err := s.githubSvc.GetLatestCommit(ctx, token, project.RepoOwner, project.RepoName, branch)
	if err != nil {
		return "", fmt.Errorf("failed to fetch latest commit: %w", err)
	}

	deployment, err := s.createDeployment(ctx, projectID, env.ID, commitSHA, commitMsg)
	if err != nil {
		return "", fmt.Errorf("failed to create deployment record: %w", err)
	}

	if err := s.enqueuer.EnqueueDeploy(
		projectID.String(),
		env.ID.String(),
		deployment.ID.String(),
		commitSHA,
	); err != nil {
		return "", fmt.Errorf("failed to queue deploy job: %w", err)
	}

	return fmt.Sprintf("Deploy started for commit `%s` — I'll stream updates as the build progresses.", commitSHA[:8]), nil
}

// RunDeployWorkflow executes the deploy pipeline. Workload topology (public service) is
// currently hardcoded; AI classification will be wired in a future iteration.
func (s *Service) RunDeployWorkflow(ctx context.Context, projectID, environmentID, deploymentID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	project, err := s.getProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}

	env, err := s.getEnvironmentByID(ctx, environmentID)
	if err != nil {
		return fmt.Errorf("environment not found: %w", err)
	}

	if env.StackStatus != models.StackStatusReady {
		return s.failDeployment(ctx, projectID, deploymentID,
			fmt.Sprintf("environment not ready (stack status: %s) — provision it first", env.StackStatus))
	}

	// Resolve networking from platform stack (new model) or legacy fields.
	clusterName, subnets, sgID, ps, err := s.resolveNetworking(ctx, env)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to resolve networking: %s", err))
	}

	if env.ECRRepoURI == nil || env.CodeBuildProjectName == nil || env.TaskExecutionRoleARN == nil || env.LogGroupName == nil {
		return s.failDeployment(ctx, projectID, deploymentID,
			"environment infrastructure not fully provisioned — re-provision if this persists")
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, env)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to assume IAM role: %s", err))
	}

	token, err := s.githubSvc.GetTokenForDeployment(ctx, project.UserID)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("GitHub not connected: %s", err))
	}

	deployment, err := s.getDeployment(ctx, deploymentID)
	if err != nil {
		return fmt.Errorf("deployment record not found: %w", err)
	}

	imageURI := fmt.Sprintf("%s:%s", *env.ECRRepoURI, deployment.CommitSHA[:8])
	ecrRegistry := strings.SplitN(*env.ECRRepoURI, "/", 2)[0]

	envIDPtr := &env.ID
	depIDPtr := &deploymentID

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventDeployStarted,
		Payload:       map[string]any{"commit_sha": deployment.CommitSHA, "image_uri": imageURI},
	})

	// Step 1 — build
	s.updateDeploymentStatus(ctx, deploymentID, models.DeployStatusBuilding, nil, nil)
	s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Starting container build..."})

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventBuildStarted,
		Source:        models.SourceBuild,
		Payload:       map[string]any{"commit_sha": deployment.CommitSHA},
	})

	startCommand := ""
	if project.StartCommand != nil {
		startCommand = *project.StartCommand
	}
	buildRes, err := s.awsSvc.StartCodeBuildJob(
		ctx, clients,
		projectID.String(),
		*env.CodeBuildProjectName,
		token,
		project.RepoOwner, project.RepoName,
		deployment.CommitSHA,
		imageURI, ecrRegistry,
		project.Framework, startCommand,
	)
	if err != nil {
		s.events.Emit(ctx, events.Event{
			ProjectID:     projectID,
			EnvironmentID: envIDPtr,
			DeploymentID:  depIDPtr,
			Type:          models.EventBuildFailed,
			Severity:      models.SeverityError,
			Source:        models.SourceBuild,
			Payload:       map[string]any{"reason": err.Error()},
		})
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to start build: %s", err))
	}
	buildID := buildRes.BuildID

	// The GitHub token is stored in SSM for the duration of the build (secure path) and
	// deleted once the build finishes, win or lose. cleanupSecret is idempotent and safe.
	cleanupSecret := func() {
		if buildRes.TokenParamName == "" {
			return
		}
		s.awsSvc.DeleteSSMParameter(ctx, clients, buildRes.TokenParamName)
		s.events.Emit(ctx, events.Event{
			ProjectID:     projectID,
			EnvironmentID: envIDPtr,
			DeploymentID:  depIDPtr,
			Type:          models.EventSecretDeleted,
			Source:        models.SourceBuild,
			Payload:       map[string]any{"param": buildRes.TokenParamName},
		})
	}
	if buildRes.SecretStored {
		s.events.Emit(ctx, events.Event{
			ProjectID:     projectID,
			EnvironmentID: envIDPtr,
			DeploymentID:  depIDPtr,
			Type:          models.EventSecretCreated,
			Source:        models.SourceBuild,
			Payload:       map[string]any{"param": buildRes.TokenParamName, "store": "ssm-securestring"},
		})
	}

	// Step 2 — wait for build
	if err = s.awsSvc.WaitForCodeBuild(ctx, clients, buildID, func(msg string) {
		s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: msg})
	}); err != nil {
		cleanupSecret()
		s.events.Emit(ctx, events.Event{
			ProjectID:     projectID,
			EnvironmentID: envIDPtr,
			DeploymentID:  depIDPtr,
			Type:          models.EventBuildFailed,
			Severity:      models.SeverityError,
			Source:        models.SourceBuild,
			Payload:       map[string]any{"build_id": buildID, "reason": err.Error()},
		})
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("build failed: %s", err))
	}
	cleanupSecret()

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventBuildCompleted,
		Source:        models.SourceBuild,
		Payload:       map[string]any{"build_id": buildID, "image_uri": imageURI},
	})

	// Step 3 — register task definition (with env vars injected)
	s.updateDeploymentStatus(ctx, deploymentID, models.DeployStatusDeploying, nil, &imageURI)
	s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Build complete. Registering task definition..."})

	envVarPairs, _ := s.envVars.LoadForEnvironment(ctx, env.ID)
	taskDefARN, err := s.awsSvc.RegisterECSTaskDefinition(ctx, clients, env, project, imageURI, envVarPairs)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to register task definition: %s", err))
	}

	// Step 4 — ensure target group (public-service topology, SDK-driven)
	s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Ensuring ALB target group..."})
	tgARN, err := s.awsSvc.EnsureTargetGroup(ctx, clients, ps, env, project)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to ensure target group: %s", err))
	}
	// Refresh env so EnsureECSService sees the TG ARN.
	env.ALBTargetGroupARN = &tgARN

	// Step 5 — ensure listener rule (SDK-driven, host-based routing foundation)
	if ps != nil {
		s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Ensuring ALB listener rule..."})
		if _, err = s.awsSvc.EnsureListenerRule(ctx, clients, ps, env, tgARN); err != nil {
			return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to ensure listener rule: %s", err))
		}
	}

	// Step 6 — create or update ECS service
	s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Deploying to ECS..."})

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventECSRolloutStarted,
		Source:        models.SourceECS,
		Payload:       map[string]any{"task_def_arn": taskDefARN, "cluster": clusterName},
	})

	if err = s.awsSvc.EnsureECSService(ctx, clients, env, project, taskDefARN, clusterName, subnets, sgID); err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to update ECS service: %s", err))
	}

	// Step 7 — wait for stabilization
	if err = s.awsSvc.WaitForECSServiceStable(ctx, clients, clusterName, env, func(msg string) {
		s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: msg})
	}); err != nil {
		s.events.Emit(ctx, events.Event{
			ProjectID:     projectID,
			EnvironmentID: envIDPtr,
			DeploymentID:  depIDPtr,
			Type:          models.EventHealthcheckFailed,
			Severity:      models.SeverityError,
			Source:        models.SourceECS,
			Payload:       map[string]any{"cluster": clusterName, "reason": err.Error()},
		})
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("deployment failed to stabilize: %s", err))
	}

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventECSStable,
		Source:        models.SourceECS,
		Payload:       map[string]any{"cluster": clusterName, "image_uri": imageURI},
	})

	// Step 8 — mark live
	s.updateDeploymentStatus(ctx, deploymentID, models.DeployStatusLive, nil, &imageURI)

	liveMsg := fmt.Sprintf("Deployment live! Commit `%s` is running.", deployment.CommitSHA[:8])
	if env.ALBDNS != nil {
		liveMsg = fmt.Sprintf("Deployment live! Commit `%s` is running at http://%s", deployment.CommitSHA[:8], *env.ALBDNS)
	}
	s.broadcast(projectID, ws.Message{Type: "deploy_done", Payload: liveMsg})

	return nil
}

// RunProvisionWorkflow sets up per-project resources using the two-tier model:
//  1. Platform stack (VPC, ECS cluster, shared ALB) — created once per account×region, reused.
//  2. Project stack (ECR, IAM roles, CodeBuild, log group) — created once per project×environment.
//
// ECS services and ALB target groups are NOT created here; they are created at deploy time so
// the platform can decide workload topology (public service, worker, cron, etc.) per deploy.
func (s *Service) RunProvisionWorkflow(ctx context.Context, projectID, environmentID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	project, err := s.getProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}

	env, err := s.getEnvironmentByID(ctx, environmentID)
	if err != nil {
		return fmt.Errorf("environment not found: %w", err)
	}

	if env.AccountID == nil {
		return fmt.Errorf("environment has no AWS account linked")
	}

	envIDPtr := &env.ID
	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		Type:          models.EventProvisionStarted,
		Payload:       map[string]any{"env_name": env.Name, "aws_region": env.AWSRegion},
	})

	s.broadcast(projectID, ws.Message{Type: "provision_progress", Payload: "Connecting to AWS..."})

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, env)
	if err != nil {
		s.markEnvFailed(ctx, environmentID)
		s.broadcast(projectID, ws.Message{Type: "provision_failed", Payload: fmt.Sprintf("Failed to assume IAM role: %s", err)})
		return err
	}

	// ── Step 1: Platform stack ────────────────────────────────────────────────
	// Find or create the platform stack record for this account+region.
	ps, isNew, err := s.awsSvc.GetOrCreatePlatformStack(ctx, *env.AccountID, env.AWSRegion)
	if err != nil {
		s.markEnvFailed(ctx, environmentID)
		s.broadcast(projectID, ws.Message{Type: "provision_failed", Payload: fmt.Sprintf("Failed to initialize platform stack: %s", err)})
		return err
	}

	if ps.StackStatus != "ready" {
		// Deploy or wait for the platform stack.
		if isNew || ps.StackStatus == "pending" || ps.StackStatus == "failed" {
			s.broadcast(projectID, ws.Message{Type: "provision_progress", Payload: "Deploying shared platform stack (VPC, ECS cluster, ALB) — first time only, ~3 min..."})

			s.db.Pool.Exec(ctx, `UPDATE platform_stacks SET stack_status = $1, updated_at = NOW() WHERE id = $2`,
				"provisioning", ps.ID)

			stackID, err := s.awsSvc.DeployPlatformStack(ctx, clients, env.AccountID.String(), env.AWSRegion)
			if err != nil {
				s.db.Pool.Exec(ctx, `UPDATE platform_stacks SET stack_status = 'failed', updated_at = NOW() WHERE id = $1`, ps.ID)
				s.markEnvFailed(ctx, environmentID)
				s.broadcast(projectID, ws.Message{Type: "provision_failed", Payload: fmt.Sprintf("Platform stack failed: %s", err)})
				return err
			}

			s.db.Pool.Exec(ctx, `UPDATE platform_stacks SET stack_id = $1, updated_at = NOW() WHERE id = $2`, stackID, ps.ID)

			err = s.awsSvc.WaitForPlatformStackAndPopulate(ctx, clients, ps, stackID, func(msg string) {
				s.broadcast(projectID, ws.Message{Type: "provision_progress", Payload: msg})
			})
			if err != nil {
				s.db.Pool.Exec(ctx, `UPDATE platform_stacks SET stack_status = 'failed', updated_at = NOW() WHERE id = $1`, ps.ID)
				s.markEnvFailed(ctx, environmentID)
				s.broadcast(projectID, ws.Message{Type: "provision_failed", Payload: fmt.Sprintf("Platform stack failed: %s", err)})
				return err
			}
		} else {
			// Another environment is currently provisioning the platform stack — unusual but safe.
			s.broadcast(projectID, ws.Message{Type: "provision_progress", Payload: "Waiting for shared platform stack to finish provisioning..."})
		}
	} else {
		s.broadcast(projectID, ws.Message{Type: "provision_progress", Payload: "Shared platform stack already ready. Provisioning project resources..."})
	}

	// ── Step 2: Project stack ─────────────────────────────────────────────────
	s.broadcast(projectID, ws.Message{Type: "provision_progress", Payload: "Deploying project resources (ECR, IAM roles, CodeBuild) — ~2 min..."})

	projectStackID, err := s.awsSvc.DeployProjectStack(ctx, clients, env, project)
	if err != nil {
		s.markEnvFailed(ctx, environmentID)
		s.broadcast(projectID, ws.Message{Type: "provision_failed", Payload: fmt.Sprintf("Project stack failed to start: %s", err)})
		return err
	}

	s.db.Pool.Exec(ctx,
		`UPDATE environments SET cloudformation_stack_id = $1, updated_at = NOW() WHERE id = $2`,
		projectStackID, environmentID,
	)

	err = s.awsSvc.WaitForProjectStackAndPopulate(ctx, clients, env, project, projectStackID, ps, func(msg string) {
		s.broadcast(projectID, ws.Message{Type: "provision_progress", Payload: msg})
	})
	if err != nil {
		s.markEnvFailed(ctx, environmentID)
		s.broadcast(projectID, ws.Message{Type: "provision_failed", Payload: fmt.Sprintf("Project resources failed: %s", err)})
		return err
	}

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		Type:          models.EventProvisionReady,
		Payload:       map[string]any{"env_name": env.Name},
	})

	s.broadcast(projectID, ws.Message{
		Type:    "provision_done",
		Payload: "Infrastructure ready! You can now deploy your first version.",
	})

	return nil
}

// stuckThresholdMinutes is how long a deployment/environment may sit in an in-progress
// state before the watchdog considers it stuck. It is deliberately larger than the 45-minute
// workflow context timeout so a legitimately running job is never falsely failed.
const stuckThresholdMinutes = 60

// ReconcileStuckResources is the watchdog entrypoint (invoked periodically by the queue
// scheduler). It marks deployments stuck in building/deploying and environments stuck in
// provisioning past the threshold as failed, emitting operational events for each. It is
// idempotent: once a row is marked failed it no longer matches, so repeated runs are safe.
func (s *Service) ReconcileStuckResources(ctx context.Context) error {
	reason := fmt.Sprintf("Automatically failed: stuck in progress for over %d minutes (the worker may have crashed).", stuckThresholdMinutes)

	// ── Stuck deployments ──────────────────────────────────────────────────────
	type stuckDep struct {
		id      uuid.UUID
		project uuid.UUID
		env     uuid.UUID
		status  string
	}
	var deps []stuckDep
	rows, err := s.db.Pool.Query(ctx,
		`UPDATE deployments
		    SET status = $1, failure_reason = $2, updated_at = NOW()
		  WHERE status IN ('building', 'deploying')
		    AND updated_at < NOW() - make_interval(mins => $3)
		 RETURNING id, project_id, environment_id, status`,
		models.DeployStatusFailed, reason, stuckThresholdMinutes,
	)
	if err != nil {
		return fmt.Errorf("watchdog: failed to scan stuck deployments: %w", err)
	}
	for rows.Next() {
		var d stuckDep
		if err := rows.Scan(&d.id, &d.project, &d.env, &d.status); err != nil {
			continue
		}
		deps = append(deps, d)
	}
	rows.Close()

	for _, d := range deps {
		envID, depID := d.env, d.id
		s.events.Emit(ctx, events.Event{
			ProjectID: d.project, EnvironmentID: &envID, DeploymentID: &depID,
			Type: models.EventDeploymentStuck, Severity: models.SeverityWarn, Source: models.SourceScheduler,
			Payload: map[string]any{"threshold_minutes": stuckThresholdMinutes},
		})
		s.events.Emit(ctx, events.Event{
			ProjectID: d.project, EnvironmentID: &envID, DeploymentID: &depID,
			Type: models.EventDeploymentAutoFailed, Severity: models.SeverityError, Source: models.SourceScheduler,
			Payload: map[string]any{"reason": reason},
		})
		s.broadcast(d.project, ws.Message{Type: "deploy_failed", Payload: reason})
	}

	// ── Stuck environments (provisioning) ──────────────────────────────────────
	type stuckEnv struct {
		id      uuid.UUID
		project uuid.UUID
	}
	var envs []stuckEnv
	rows2, err := s.db.Pool.Query(ctx,
		`UPDATE environments
		    SET stack_status = $1, updated_at = NOW()
		  WHERE stack_status = 'provisioning'
		    AND updated_at < NOW() - make_interval(mins => $2)
		 RETURNING id, project_id`,
		models.StackStatusFailed, stuckThresholdMinutes,
	)
	if err != nil {
		return fmt.Errorf("watchdog: failed to scan stuck environments: %w", err)
	}
	for rows2.Next() {
		var e stuckEnv
		if err := rows2.Scan(&e.id, &e.project); err != nil {
			continue
		}
		envs = append(envs, e)
	}
	rows2.Close()

	for _, e := range envs {
		envID := e.id
		s.events.Emit(ctx, events.Event{
			ProjectID: e.project, EnvironmentID: &envID,
			Type: models.EventProvisionFailed, Severity: models.SeverityError, Source: models.SourceScheduler,
			Payload: map[string]any{"reason": reason, "threshold_minutes": stuckThresholdMinutes},
		})
		s.broadcast(e.project, ws.Message{Type: "provision_failed", Payload: reason})
	}

	if len(deps) > 0 || len(envs) > 0 {
		log.Printf("[watchdog] auto-failed %d stuck deployment(s) and %d stuck environment(s)", len(deps), len(envs))
	}
	return nil
}

func (s *Service) markEnvFailed(ctx context.Context, environmentID uuid.UUID) {
	s.db.Pool.Exec(ctx,
		`UPDATE environments SET stack_status = $1, updated_at = NOW() WHERE id = $2`,
		models.StackStatusFailed, environmentID,
	)
}

// TriggerRollback rolls the environment back to the previous successful image. It finds
// the two most recent deployments that produced a runnable image, creates a new deployment
// record pinned to the previous one's image, and enqueues a rollback job (no rebuild).
func (s *Service) TriggerRollback(ctx context.Context, projectID uuid.UUID, envName string) (string, error) {
	env, err := s.getEnvironment(ctx, projectID, envName)
	if err != nil {
		return "", fmt.Errorf("environment %q not found", envName)
	}

	deployable, err := s.getRecentDeployableDeployments(ctx, env.ID, 2)
	if err != nil {
		return "", fmt.Errorf("failed to load deployment history: %w", err)
	}
	if len(deployable) < 2 {
		return "Nothing to roll back to — this environment needs at least one previous successful deployment.", nil
	}

	current := deployable[0] // most recent good deployment (rolling back from)
	target := deployable[1]  // the one before it (rolling back to)
	if target.ImageURI == nil {
		return "The previous deployment has no image to roll back to.", nil
	}

	commitMsg := ""
	if target.CommitMessage != nil {
		commitMsg = *target.CommitMessage
	}
	newDep, err := s.createDeploymentWithImage(ctx, projectID, env.ID, target.CommitSHA, commitMsg, *target.ImageURI)
	if err != nil {
		return "", fmt.Errorf("failed to create rollback deployment record: %w", err)
	}

	if err := s.enqueuer.EnqueueRollback(
		projectID.String(), env.ID.String(), newDep.ID.String(), current.ID.String(),
	); err != nil {
		return "", fmt.Errorf("failed to queue rollback job: %w", err)
	}

	return fmt.Sprintf("Rolling back %s to commit `%s` — I'll stream updates as it deploys.", envName, target.CommitSHA[:8]), nil
}

// FetchLogsForProject returns recent application log lines for the project's deployable
// environment, fetched live from CloudWatch.
func (s *Service) FetchLogsForProject(ctx context.Context, projectID uuid.UUID) (string, error) {
	env, err := s.getDeployableEnvironment(ctx, projectID)
	if err != nil {
		return "", err
	}
	if env.LogGroupName == nil {
		return "That environment isn't provisioned yet — no logs to show.", nil
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, env)
	if err != nil {
		return "", fmt.Errorf("failed to connect to AWS: %w", err)
	}

	lines, err := s.awsSvc.FetchRecentECSLogs(ctx, clients, *env.LogGroupName, 100)
	if err != nil {
		return "", fmt.Errorf("failed to fetch logs: %w", err)
	}
	if len(lines) == 0 {
		return fmt.Sprintf("No recent logs found for the %s environment.", env.Name), nil
	}

	// Show the last ~40 lines in chat; the Logs tab has the full view.
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return fmt.Sprintf("Recent logs from **%s**:\n```\n%s\n```", env.Name, strings.Join(lines, "\n")), nil
}

// CheckHealth describes the ECS service for the project's deployable environment and
// returns a human-readable running/desired summary plus rollout state.
func (s *Service) CheckHealth(ctx context.Context, projectID uuid.UUID) (string, error) {
	env, err := s.getDeployableEnvironment(ctx, projectID)
	if err != nil {
		return "", err
	}
	if env.ECSServiceName == nil {
		return fmt.Sprintf("The %s environment hasn't been deployed yet — nothing running to check.", env.Name), nil
	}

	clusterName, _, _, _, err := s.resolveNetworking(ctx, env)
	if err != nil {
		return "", fmt.Errorf("failed to resolve cluster: %w", err)
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, env)
	if err != nil {
		return "", fmt.Errorf("failed to connect to AWS: %w", err)
	}

	h, err := s.awsSvc.DescribeECSService(ctx, clients, clusterName, *env.ECSServiceName)
	if err != nil {
		return "", err
	}

	status := "healthy"
	if h.RunningCount < h.DesiredCount {
		status = "degraded"
	}
	if h.RunningCount == 0 && h.DesiredCount > 0 {
		status = "down"
	}

	msg := fmt.Sprintf("**%s** is %s — %d/%d tasks running", env.Name, status, h.RunningCount, h.DesiredCount)
	if h.PendingCount > 0 {
		msg += fmt.Sprintf(" (%d pending)", h.PendingCount)
	}
	if h.RolloutState != "" && h.RolloutState != "COMPLETED" {
		msg += fmt.Sprintf(". Rollout: %s", h.RolloutState)
		if h.Reason != "" {
			msg += fmt.Sprintf(" — %s", h.Reason)
		}
	}
	if env.ALBDNS != nil {
		msg += fmt.Sprintf("\nURL: http://%s", *env.ALBDNS)
	}
	return msg, nil
}

// ScaleService updates the ECS service desired count for the project's deployable environment.
func (s *Service) ScaleService(ctx context.Context, projectID uuid.UUID, replicas int) (string, error) {
	if replicas < 0 || replicas > 10 {
		return "Replica count must be between 0 and 10.", nil
	}

	env, err := s.getDeployableEnvironment(ctx, projectID)
	if err != nil {
		return "", err
	}
	if env.ECSServiceName == nil {
		return fmt.Sprintf("The %s environment hasn't been deployed yet — deploy before scaling.", env.Name), nil
	}

	clusterName, _, _, _, err := s.resolveNetworking(ctx, env)
	if err != nil {
		return "", fmt.Errorf("failed to resolve cluster: %w", err)
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, env)
	if err != nil {
		return "", fmt.Errorf("failed to connect to AWS: %w", err)
	}

	if err := s.awsSvc.UpdateServiceDesiredCount(ctx, clients, clusterName, *env.ECSServiceName, int32(replicas)); err != nil {
		return "", err
	}

	return fmt.Sprintf("Scaling **%s** to %d replica(s) — ECS is rolling the change out now.", env.Name, replicas), nil
}

// RunRollbackWorkflow re-deploys an already-built image (no CodeBuild) to the ECS service.
// deploymentID is the new rollback deployment record (its image_uri is the rollback target);
// previousDeploymentID, if set, is marked rolled_back once the new image is live.
func (s *Service) RunRollbackWorkflow(ctx context.Context, projectID, environmentID, deploymentID uuid.UUID, previousDeploymentID *uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	project, err := s.getProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}
	env, err := s.getEnvironmentByID(ctx, environmentID)
	if err != nil {
		return fmt.Errorf("environment not found: %w", err)
	}
	deployment, err := s.getDeployment(ctx, deploymentID)
	if err != nil {
		return fmt.Errorf("deployment record not found: %w", err)
	}
	if deployment.ImageURI == nil {
		return s.failDeployment(ctx, projectID, deploymentID, "rollback target has no image URI")
	}

	if env.StackStatus != models.StackStatusReady {
		return s.failDeployment(ctx, projectID, deploymentID,
			fmt.Sprintf("environment not ready (stack status: %s)", env.StackStatus))
	}
	if env.TaskExecutionRoleARN == nil || env.LogGroupName == nil {
		return s.failDeployment(ctx, projectID, deploymentID, "environment infrastructure not fully provisioned")
	}

	clusterName, subnets, sgID, ps, err := s.resolveNetworking(ctx, env)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to resolve networking: %s", err))
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, env)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to assume IAM role: %s", err))
	}

	imageURI := *deployment.ImageURI
	envIDPtr := &env.ID
	depIDPtr := &deploymentID

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventRollbackTriggered,
		ActorType:     models.ActorUser,
		Payload:       map[string]any{"image_uri": imageURI, "commit_sha": deployment.CommitSHA},
	})

	s.updateDeploymentStatus(ctx, deploymentID, models.DeployStatusDeploying, nil, &imageURI)
	s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Rolling back — registering previous task definition..."})

	// Re-inject current env vars so the rolled-back image still gets the latest config.
	envVarPairs, _ := s.envVars.LoadForEnvironment(ctx, env.ID)
	taskDefARN, err := s.awsSvc.RegisterECSTaskDefinition(ctx, clients, env, project, imageURI, envVarPairs)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to register task definition: %s", err))
	}

	s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Ensuring ALB target group..."})
	tgARN, err := s.awsSvc.EnsureTargetGroup(ctx, clients, ps, env, project)
	if err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to ensure target group: %s", err))
	}
	env.ALBTargetGroupARN = &tgARN

	if ps != nil {
		if _, err = s.awsSvc.EnsureListenerRule(ctx, clients, ps, env, tgARN); err != nil {
			return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to ensure listener rule: %s", err))
		}
	}

	s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Deploying previous version to ECS..."})
	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventECSRolloutStarted,
		Source:        models.SourceECS,
		Payload:       map[string]any{"task_def_arn": taskDefARN, "cluster": clusterName, "rollback": true},
	})

	if err = s.awsSvc.EnsureECSService(ctx, clients, env, project, taskDefARN, clusterName, subnets, sgID); err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("failed to update ECS service: %s", err))
	}

	if err = s.awsSvc.WaitForECSServiceStable(ctx, clients, clusterName, env, func(msg string) {
		s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: msg})
	}); err != nil {
		return s.failDeployment(ctx, projectID, deploymentID, fmt.Sprintf("rollback failed to stabilize: %s", err))
	}

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventECSStable,
		Source:        models.SourceECS,
		Payload:       map[string]any{"cluster": clusterName, "image_uri": imageURI, "rollback": true},
	})

	s.updateDeploymentStatus(ctx, deploymentID, models.DeployStatusLive, nil, &imageURI)

	// Mark the deployment we rolled back from as rolled_back.
	if previousDeploymentID != nil {
		s.db.Pool.Exec(ctx,
			`UPDATE deployments SET status = $1, updated_at = NOW() WHERE id = $2`,
			models.DeployStatusRolledBack, *previousDeploymentID,
		)
	}

	liveMsg := fmt.Sprintf("Rolled back! Commit `%s` is live again.", deployment.CommitSHA[:8])
	if env.ALBDNS != nil {
		liveMsg = fmt.Sprintf("Rolled back! Commit `%s` is live again at http://%s", deployment.CommitSHA[:8], *env.ALBDNS)
	}
	s.broadcast(projectID, ws.Message{Type: "deploy_done", Payload: liveMsg})
	return nil
}

// HandleDeleteProject removes a project and enqueues background AWS cleanup.
// Flow: snapshot all AWS resource refs from DB → delete DB record (cascade) → enqueue cleanup.
// The project disappears from the API immediately; AWS teardown is best-effort.
func (s *Service) HandleDeleteProject(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	project, err := s.getProject(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	// Snapshot every environment's AWS resource references BEFORE deleting the DB record.
	envCleanupInfos, err := s.collectEnvCleanupInfos(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to collect environment info: " + err.Error()})
		return
	}

	// Delete the DB record — cascade removes environments, deployments, conversations,
	// operational_events, env_vars, incidents.
	if _, err = s.db.Pool.Exec(c.Request.Context(),
		`DELETE FROM projects WHERE id = $1`, projectID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete project"})
		return
	}

	// Enqueue background AWS cleanup (best-effort; DB record is already gone).
	if len(envCleanupInfos) > 0 {
		payload := DeleteProjectPayload{
			ProjectName:  project.Name,
			Environments: envCleanupInfos,
		}
		if err := s.enqueuer.EnqueueDeleteProject(payload); err != nil {
			// Log but don't fail the HTTP response — the DB record is already deleted.
			log.Printf("[delete-project] failed to enqueue AWS cleanup for %s: %v", project.Name, err)
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Project deleted"})
}

// collectEnvCleanupInfos snapshots all AWS resource references for every environment of a
// project so the background cleanup job has everything it needs after the DB record is gone.
func (s *Service) collectEnvCleanupInfos(ctx context.Context, projectID uuid.UUID) ([]DeleteEnvCleanupInfo, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT e.id, e.aws_region,
		        a.iam_role_arn, a.external_id,
		        e.cloudformation_stack_id,
		        e.ecr_repo_uri,
		        e.ecs_service_name,
		        e.alb_listener_rule_arn,
		        e.alb_target_group_arn,
		        -- cluster name: prefer platform stack, fall back to env field
		        COALESCE(ps.ecs_cluster_name, e.ecs_cluster_name) AS cluster_name
		 FROM environments e
		 LEFT JOIN aws_accounts a ON a.id = e.account_id
		 LEFT JOIN platform_stacks ps ON ps.id = e.platform_stack_id
		 WHERE e.project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var infos []DeleteEnvCleanupInfo
	for rows.Next() {
		var (
			envID                              string
			region                             string
			iamRole, extID                     *string
			cfStackID                          *string
			ecrURI, ecsService                 *string
			albRule, albTG                     *string
			clusterName                        *string
		)
		if err := rows.Scan(&envID, &region, &iamRole, &extID, &cfStackID,
			&ecrURI, &ecsService, &albRule, &albTG, &clusterName); err != nil {
			continue
		}
		if iamRole == nil {
			continue // no AWS account linked — nothing to clean up
		}

		info := DeleteEnvCleanupInfo{
			Region:     region,
			IAMRoleARN: deref(iamRole),
			ExternalID: deref(extID),
		}
		if cfStackID != nil {
			info.ProjectStackID = *cfStackID
		}
		if ecrURI != nil {
			// Extract the short repo name from the full URI (account.dkr.ecr.region.amazonaws.com/name)
			parts := strings.SplitN(*ecrURI, "/", 2)
			if len(parts) == 2 {
				info.ECRRepoName = parts[1]
			}
		}
		if clusterName != nil {
			info.ECSClusterName = *clusterName
		}
		if ecsService != nil {
			info.ECSServiceName = *ecsService
		}
		if albRule != nil {
			info.ALBListenerRuleARN = *albRule
		}
		if albTG != nil {
			info.ALBTargetGroupARN = *albTG
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// RunDeleteProjectCleanup runs the best-effort AWS teardown for a deleted project.
// Each step is idempotent and independent — one failure does not abort the others.
func (s *Service) RunDeleteProjectCleanup(ctx context.Context, payload DeleteProjectPayload) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	for _, env := range payload.Environments {
		log.Printf("[delete-project] cleaning up env region=%s stack=%s", env.Region, env.ProjectStackID)
		if err := s.cleanupEnv(ctx, env); err != nil {
			// Log and continue — other environments should still be cleaned up.
			log.Printf("[delete-project] cleanup error for env region=%s: %v", env.Region, err)
		}
	}
	log.Printf("[delete-project] cleanup complete for %q", payload.ProjectName)
	return nil
}

func (s *Service) cleanupEnv(ctx context.Context, env DeleteEnvCleanupInfo) error {
	clients, err := s.awsSvc.AssumeRoleForAccount(ctx, env.IAMRoleARN, env.ExternalID, env.Region)
	if err != nil {
		return fmt.Errorf("assume role: %w", err)
	}

	// 1. Delete ECS service (scale to 0 first, then force-delete).
	if env.ECSClusterName != "" && env.ECSServiceName != "" {
		if err := s.awsSvc.DeleteECSService(ctx, clients, env.ECSClusterName, env.ECSServiceName); err != nil {
			log.Printf("[delete-project] DeleteECSService: %v", err)
		}
	}

	// 2. Delete ALB listener rule.
	if err := s.awsSvc.DeleteListenerRule(ctx, clients, env.ALBListenerRuleARN); err != nil {
		log.Printf("[delete-project] DeleteListenerRule: %v", err)
	}

	// 3. Delete ALB target group.
	if err := s.awsSvc.DeleteTargetGroup(ctx, clients, env.ALBTargetGroupARN); err != nil {
		log.Printf("[delete-project] DeleteTargetGroup: %v", err)
	}

	// 4. Purge ECR images so CloudFormation can delete the repository.
	if env.ECRRepoName != "" {
		if err := s.awsSvc.PurgeECRRepository(ctx, clients, env.ECRRepoName); err != nil {
			log.Printf("[delete-project] PurgeECRRepository: %v", err)
		}
	}

	// 5. Delete the CloudFormation project stack (ECR repo, IAM roles, CodeBuild, log group).
	if env.ProjectStackID != "" {
		if err := s.awsSvc.DeleteProjectStack(ctx, clients, env.ProjectStackID); err != nil {
			log.Printf("[delete-project] DeleteProjectStack: %v", err)
		}
	}

	return nil
}

// HandleGetLogs streams recent ECS application logs for an environment from CloudWatch.
func (s *Service) HandleGetLogs(c *gin.Context) {
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

	lines := int32(200)
	if l := c.Query("lines"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
			lines = int32(n)
		}
	}

	env, err := s.getEnvironmentByID(c.Request.Context(), envID)
	if err != nil || env.ProjectID != projectID {
		c.JSON(http.StatusNotFound, gin.H{"error": "environment not found"})
		return
	}
	if env.LogGroupName == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "environment not yet provisioned"})
		return
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(c.Request.Context(), env)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to connect to AWS: " + err.Error()})
		return
	}

	logs, err := s.awsSvc.FetchRecentECSLogs(c.Request.Context(), clients, *env.LogGroupName, lines)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch logs: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"lines": logs, "log_group": *env.LogGroupName})
}

// ---- private DB helpers ----

func (s *Service) getProject(ctx context.Context, projectID uuid.UUID) (*models.Project, error) {
	var p models.Project
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, user_id, name, repo_url, repo_owner, repo_name, framework, branch, start_command, account_id, created_at, updated_at
		 FROM projects WHERE id = $1`, projectID,
	).Scan(&p.ID, &p.UserID, &p.Name, &p.RepoURL, &p.RepoOwner,
		&p.RepoName, &p.Framework, &p.Branch, &p.StartCommand, &p.AccountID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Service) getEnvironment(ctx context.Context, projectID uuid.UUID, envName string) (*models.Environment, error) {
	var env models.Environment
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, project_id, name, aws_region, account_id, platform_stack_id,
		        cloudformation_stack_id, stack_status, alb_dns,
		        ecr_repo_uri, ecs_cluster_name, ecs_service_name,
		        codebuild_project_name, task_execution_role_arn, log_group_name,
		        alb_target_group_arn, alb_listener_rule_arn, ecs_security_group_id, vpc_subnets,
		        created_at, updated_at
		 FROM environments WHERE project_id = $1 AND name = $2`, projectID, envName,
	).Scan(
		&env.ID, &env.ProjectID, &env.Name, &env.AWSRegion,
		&env.AccountID, &env.PlatformStackID,
		&env.CloudFormationStackID, &env.StackStatus, &env.ALBDNS,
		&env.ECRRepoURI, &env.ECSClusterName, &env.ECSServiceName,
		&env.CodeBuildProjectName, &env.TaskExecutionRoleARN, &env.LogGroupName,
		&env.ALBTargetGroupARN, &env.ALBListenerRuleARN, &env.ECSSecurityGroupID, &env.VPCSubnets,
		&env.CreatedAt, &env.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &env, nil
}

func (s *Service) getEnvironmentByID(ctx context.Context, envID uuid.UUID) (*models.Environment, error) {
	var env models.Environment
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, project_id, name, aws_region, account_id, platform_stack_id,
		        cloudformation_stack_id, stack_status, alb_dns,
		        ecr_repo_uri, ecs_cluster_name, ecs_service_name,
		        codebuild_project_name, task_execution_role_arn, log_group_name,
		        alb_target_group_arn, alb_listener_rule_arn, ecs_security_group_id, vpc_subnets,
		        created_at, updated_at
		 FROM environments WHERE id = $1`, envID,
	).Scan(
		&env.ID, &env.ProjectID, &env.Name, &env.AWSRegion,
		&env.AccountID, &env.PlatformStackID,
		&env.CloudFormationStackID, &env.StackStatus, &env.ALBDNS,
		&env.ECRRepoURI, &env.ECSClusterName, &env.ECSServiceName,
		&env.CodeBuildProjectName, &env.TaskExecutionRoleARN, &env.LogGroupName,
		&env.ALBTargetGroupARN, &env.ALBListenerRuleARN, &env.ECSSecurityGroupID, &env.VPCSubnets,
		&env.CreatedAt, &env.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &env, nil
}

func (s *Service) getDeployment(ctx context.Context, deploymentID uuid.UUID) (*models.Deployment, error) {
	var d models.Deployment
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, project_id, environment_id, commit_sha, commit_message, image_uri, status, failure_reason, created_at, updated_at
		 FROM deployments WHERE id = $1`, deploymentID,
	).Scan(&d.ID, &d.ProjectID, &d.EnvironmentID, &d.CommitSHA, &d.CommitMessage,
		&d.ImageURI, &d.Status, &d.FailureReason, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// getDeployableEnvironment returns the environment to act on for conversational commands
// (logs/health/scale). It prefers production, then staging, then any provisioned environment.
func (s *Service) getDeployableEnvironment(ctx context.Context, projectID uuid.UUID) (*models.Environment, error) {
	for _, name := range []string{"production", "staging"} {
		if env, err := s.getEnvironment(ctx, projectID, name); err == nil && env.StackStatus == models.StackStatusReady {
			return env, nil
		}
	}
	// Fall back to any ready environment.
	for _, name := range []string{"production", "staging"} {
		if env, err := s.getEnvironment(ctx, projectID, name); err == nil {
			return env, nil
		}
	}
	return nil, fmt.Errorf("no environment found — create and provision one first")
}

// getRecentDeployableDeployments returns up to `limit` most recent deployments for an
// environment that produced a runnable image (have image_uri and reached live or rolled_back),
// newest first. Used to find rollback targets.
func (s *Service) getRecentDeployableDeployments(ctx context.Context, envID uuid.UUID, limit int) ([]models.Deployment, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, project_id, environment_id, commit_sha, commit_message, image_uri, status, failure_reason, created_at, updated_at
		 FROM deployments
		 WHERE environment_id = $1 AND image_uri IS NOT NULL AND status IN ('live', 'rolled_back')
		 ORDER BY created_at DESC LIMIT $2`, envID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deps []models.Deployment
	for rows.Next() {
		var d models.Deployment
		if err := rows.Scan(
			&d.ID, &d.ProjectID, &d.EnvironmentID, &d.CommitSHA, &d.CommitMessage,
			&d.ImageURI, &d.Status, &d.FailureReason, &d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			continue
		}
		deps = append(deps, d)
	}
	return deps, nil
}

// createDeploymentWithImage creates a deployment record with a pre-set image URI (used by
// rollback, which reuses an already-built image rather than rebuilding).
func (s *Service) createDeploymentWithImage(ctx context.Context, projectID, envID uuid.UUID, commitSHA, commitMsg, imageURI string) (*models.Deployment, error) {
	d := &models.Deployment{
		ProjectID:     projectID,
		EnvironmentID: envID,
		CommitSHA:     commitSHA,
		CommitMessage: &commitMsg,
		ImageURI:      &imageURI,
		Status:        models.DeployStatusPending,
	}

	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO deployments (project_id, environment_id, commit_sha, commit_message, image_uri, status)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at, updated_at`,
		d.ProjectID, d.EnvironmentID, d.CommitSHA, d.CommitMessage, d.ImageURI, d.Status,
	).Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Service) createDeployment(ctx context.Context, projectID, envID uuid.UUID, commitSHA, commitMsg string) (*models.Deployment, error) {
	d := &models.Deployment{
		ProjectID:     projectID,
		EnvironmentID: envID,
		CommitSHA:     commitSHA,
		CommitMessage: &commitMsg,
		Status:        models.DeployStatusPending,
	}

	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO deployments (project_id, environment_id, commit_sha, commit_message, status)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at, updated_at`,
		d.ProjectID, d.EnvironmentID, d.CommitSHA, d.CommitMessage, d.Status,
	).Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Service) updateDeploymentStatus(ctx context.Context, deploymentID uuid.UUID, status string, failureReason, imageURI *string) {
	s.db.Pool.Exec(ctx,
		`UPDATE deployments
		 SET status = $1, failure_reason = COALESCE($2, failure_reason),
		     image_uri = COALESCE($3, image_uri), updated_at = NOW()
		 WHERE id = $4`,
		status, failureReason, imageURI, deploymentID,
	)
}

// failDeployment marks the deployment failed, broadcasts the error, and returns the error.
func (s *Service) failDeployment(ctx context.Context, projectID, deploymentID uuid.UUID, reason string) error {
	s.updateDeploymentStatus(ctx, deploymentID, models.DeployStatusFailed, &reason, nil)
	s.broadcast(projectID, ws.Message{Type: "deploy_failed", Payload: reason})
	return fmt.Errorf("%s", reason)
}

func (s *Service) broadcast(projectID uuid.UUID, msg ws.Message) {
	s.hub.Broadcast(projectID.String(), msg)
}

// resolveNetworking returns the ECS cluster name, subnet IDs, and security group ID for
// a deploy workflow. For new environments, it reads these from the platform stack.
// For legacy single-stack environments, it falls back to the fields stored on the environment.
func (s *Service) resolveNetworking(ctx context.Context, env *models.Environment) (clusterName string, subnets []string, sgID string, ps *models.PlatformStack, err error) {
	if env.PlatformStackID != nil {
		ps, err = s.awsSvc.GetPlatformStack(ctx, *env.PlatformStackID)
		if err != nil {
			return "", nil, "", nil, fmt.Errorf("platform stack not found: %w", err)
		}
		if ps.ECSClusterName == nil || ps.SubnetIDs == nil || ps.ECSSecurityGroupID == nil {
			return "", nil, "", nil, fmt.Errorf("platform stack not fully provisioned")
		}
		return *ps.ECSClusterName, strings.Split(*ps.SubnetIDs, ","), *ps.ECSSecurityGroupID, ps, nil
	}

	// Legacy: single-stack model with networking stored directly on the environment.
	if env.ECSClusterName == nil || env.VPCSubnets == nil || env.ECSSecurityGroupID == nil {
		return "", nil, "", nil, fmt.Errorf("networking not set on environment — re-provision if this persists")
	}
	return *env.ECSClusterName, strings.Split(*env.VPCSubnets, ","), *env.ECSSecurityGroupID, nil, nil
}
