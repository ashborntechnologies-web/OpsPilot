package deploy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	awssvc "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	githubsvc "github.com/ashborntechnologies-web/OpsPilot/internal/github"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Enqueuer is implemented by queue.Client. Defined here to avoid a circular import
// (the queue package imports this package for the worker).
type Enqueuer interface {
	EnqueueDeploy(projectID, environmentID, deploymentID, commitSHA string) error
	EnqueueProvision(projectID, environmentID string) error
}

type Service struct {
	db        *models.DB
	awsSvc    *awssvc.Service
	githubSvc *githubsvc.Service
	hub       *ws.Hub
	enqueuer  Enqueuer
	events    *events.Service
}

func NewService(db *models.DB, awsSvc *awssvc.Service, githubSvc *githubsvc.Service, hub *ws.Hub, enqueuer Enqueuer, eventSvc *events.Service) *Service {
	return &Service{db: db, awsSvc: awsSvc, githubSvc: githubSvc, hub: hub, enqueuer: enqueuer, events: eventSvc}
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

	response, err := s.TriggerRollback(c.Request.Context(), projectID, "production")
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
	buildID, err := s.awsSvc.StartCodeBuildJob(
		ctx, clients,
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

	// Step 2 — wait for build
	if err = s.awsSvc.WaitForCodeBuild(ctx, clients, buildID, func(msg string) {
		s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: msg})
	}); err != nil {
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

	s.events.Emit(ctx, events.Event{
		ProjectID:     projectID,
		EnvironmentID: envIDPtr,
		DeploymentID:  depIDPtr,
		Type:          models.EventBuildCompleted,
		Source:        models.SourceBuild,
		Payload:       map[string]any{"build_id": buildID, "image_uri": imageURI},
	})

	// Step 3 — register task definition
	s.updateDeploymentStatus(ctx, deploymentID, models.DeployStatusDeploying, nil, &imageURI)
	s.broadcast(projectID, ws.Message{Type: "deploy_progress", Payload: "Build complete. Registering task definition..."})

	taskDefARN, err := s.awsSvc.RegisterECSTaskDefinition(ctx, clients, env, project, imageURI)
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

func (s *Service) markEnvFailed(ctx context.Context, environmentID uuid.UUID) {
	s.db.Pool.Exec(ctx,
		`UPDATE environments SET stack_status = $1, updated_at = NOW() WHERE id = $2`,
		models.StackStatusFailed, environmentID,
	)
}

// TriggerRollback rolls back to the previous live deployment (item 6).
func (s *Service) TriggerRollback(ctx context.Context, projectID uuid.UUID, envName string) (string, error) {
	// TODO: fetch previous live deployment → re-register its task definition → update service
	return "Rollback initiated — this will be implemented in the next iteration.", nil
}

// FetchLogsForProject returns recent log lines (item 6).
func (s *Service) FetchLogsForProject(ctx context.Context, projectID uuid.UUID) (string, error) {
	// TODO: get environment → assume role → list CloudWatch log streams → fetch latest
	return "Log fetching will be wired in the next iteration.", nil
}

// CheckHealth returns current deployment health (item 6).
func (s *Service) CheckHealth(ctx context.Context, projectID uuid.UUID) (string, error) {
	// TODO: describe ECS service → return running count, task health
	return "Health check will be wired in the next iteration.", nil
}

// ScaleService updates the ECS service desired count (item 6).
func (s *Service) ScaleService(ctx context.Context, projectID uuid.UUID, replicas int) (string, error) {
	// TODO: assume role → update ECS service desired count
	return fmt.Sprintf("Scaling to %d replicas will be wired in the next iteration.", replicas), nil
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
	return fmt.Errorf(reason)
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
