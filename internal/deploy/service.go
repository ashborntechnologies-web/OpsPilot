package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	awssvc "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/envvars"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	githubsvc "github.com/ashborntechnologies-web/OpsPilot/internal/github"
	"github.com/ashborntechnologies-web/OpsPilot/internal/webhooks"
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
	db          *models.DB
	awsSvc      awssvc.AWSProvider
	githubSvc   githubsvc.GitHubProvider
	hub         *ws.Hub
	enqueuer    Enqueuer
	events      *events.Service
	envVars     *envvars.Service
	webhooksSvc *webhooks.Service

	// pendingMutations holds infra changes proposed via chat that are waiting for confirmation.
	// Keyed by projectID; TTL of 10 minutes enforced at read time.
	pendingMutations map[uuid.UUID]*pendingMutation
	mutationMu       sync.Mutex
}

type pendingMutation struct {
	proposal   *models.MutationProposal
	proposedAt time.Time
}

func NewService(db *models.DB, awsSvc awssvc.AWSProvider, githubSvc githubsvc.GitHubProvider, hub *ws.Hub, enqueuer Enqueuer, eventSvc *events.Service, envVarSvc *envvars.Service, webhookSvc *webhooks.Service) *Service {
	return &Service{
		db:               db,
		awsSvc:           awsSvc,
		githubSvc:        githubSvc,
		hub:              hub,
		enqueuer:         enqueuer,
		events:           eventSvc,
		envVars:          envVarSvc,
		webhooksSvc:      webhookSvc,
		pendingMutations: make(map[uuid.UUID]*pendingMutation),
	}
}

// ---- HTTP handlers ----

var (
	// projectNameRe permits names safe to embed in AWS resource names
	// (CloudFormation stacks, ECS services, log groups).
	projectNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 _.-]{0,62}$`)
	// githubNameRe matches valid GitHub owner/repo segments.
	githubNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$`)
)

// isNonPublicURL reports whether the URL's host is loopback, private, or link-local —
// i.e. somewhere GitHub's webhook delivery cannot reach.
func isNonPublicURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return false
}

func validateProjectInput(name, repoOwner, repoName string, branch *string) error {
	if !projectNameRe.MatchString(name) {
		return fmt.Errorf("project name must be 1-63 characters: letters, numbers, spaces, dots, dashes, underscores")
	}
	if !githubNameRe.MatchString(repoOwner) {
		return fmt.Errorf("invalid repository owner")
	}
	if !githubNameRe.MatchString(repoName) {
		return fmt.Errorf("invalid repository name")
	}
	if branch != nil && *branch != "" {
		if len(*branch) > 255 || strings.ContainsAny(*branch, " \t\n~^:?*[\\") || strings.Contains(*branch, "..") {
			return fmt.Errorf("invalid branch name")
		}
	}
	return nil
}

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

	req.Name = strings.TrimSpace(req.Name)
	if err := validateProjectInput(req.Name, req.RepoOwner, req.RepoName, req.Branch); err != nil {
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
		status := http.StatusInternalServerError
		switch {
		case strings.Contains(err.Error(), "not found"):
			status = http.StatusNotFound
		case strings.Contains(err.Error(), "already in progress"):
			status = http.StatusConflict
		case strings.Contains(err.Error(), "GitHub not connected"):
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
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

	if inFlight, _ := s.hasInFlightDeployment(c.Request.Context(), dep.EnvironmentID); inFlight {
		c.JSON(http.StatusConflict, gin.H{"error": "a deployment is already in progress for this environment"})
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
		reason := "failed to queue redeploy job"
		s.updateDeploymentStatus(c.Request.Context(), newDep.ID, models.DeployStatusFailed, &reason, nil)
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

	if inFlight, _ := s.hasInFlightDeployment(ctx, env.ID); inFlight {
		return "", fmt.Errorf("a deployment is already in progress for %s — wait for it to finish or for the watchdog to clear it", envName)
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
		// Mark the orphaned record failed so it doesn't block future deploys.
		reason := "failed to queue deploy job"
		s.updateDeploymentStatus(ctx, deployment.ID, models.DeployStatusFailed, &reason, nil)
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
	if s.webhooksSvc != nil {
		commitMsg := ""
		if deployment.CommitMessage != nil {
			commitMsg = *deployment.CommitMessage
		}
		s.webhooksSvc.FireEvent(projectID, models.WebhookEventDeployStarted,
			webhooks.BuildPayload(projectID.String(), project.Name, env.Name, deploymentID.String(), deployment.CommitSHA, commitMsg))
	}

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
	// It uses a detached context: the most common failure path is the workflow context
	// expiring, and the delete must still go through or the token leaks in SSM.
	cleanupSecret := func() {
		if buildRes.TokenParamName == "" {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.awsSvc.DeleteSSMParameter(cleanupCtx, clients, buildRes.TokenParamName)
		s.events.Emit(cleanupCtx, events.Event{
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

	if s.webhooksSvc != nil {
		commitMsg := ""
		if deployment.CommitMessage != nil {
			commitMsg = *deployment.CommitMessage
		}
		s.webhooksSvc.FireEvent(projectID, models.WebhookEventDeploySucceeded,
			webhooks.BuildPayload(projectID.String(), project.Name, env.Name, deploymentID.String(), deployment.CommitSHA, commitMsg))
	}

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
		  WHERE status IN ('pending', 'building', 'deploying')
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

	if inFlight, _ := s.hasInFlightDeployment(ctx, env.ID); inFlight {
		return "", fmt.Errorf("a deployment is already in progress for %s — wait for it to finish before rolling back", envName)
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
		reason := "failed to queue rollback job"
		s.updateDeploymentStatus(ctx, newDep.ID, models.DeployStatusFailed, &reason, nil)
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

// HandleCheckHealth returns ECS service health for a specific environment.
func (s *Service) HandleCheckHealth(c *gin.Context) {
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

	env, err := s.getEnvironmentByID(c.Request.Context(), envID)
	if err != nil || env.ProjectID != projectID {
		c.JSON(http.StatusNotFound, gin.H{"error": "environment not found"})
		return
	}
	if env.ECSServiceName == nil {
		c.JSON(http.StatusOK, gin.H{"status": "not_deployed", "running": 0, "desired": 0, "pending": 0})
		return
	}

	clusterName, _, _, _, err := s.resolveNetworking(c.Request.Context(), env)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve cluster: " + err.Error()})
		return
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(c.Request.Context(), env)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to connect to AWS: " + err.Error()})
		return
	}

	h, err := s.awsSvc.DescribeECSService(c.Request.Context(), clients, clusterName, *env.ECSServiceName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	status := "healthy"
	if h.RunningCount < h.DesiredCount {
		status = "degraded"
	}
	if h.RunningCount == 0 && h.DesiredCount > 0 {
		status = "down"
	}

	resp := gin.H{
		"status":  status,
		"running": h.RunningCount,
		"desired": h.DesiredCount,
		"pending": h.PendingCount,
	}
	if env.ALBDNS != nil {
		resp["url"] = "http://" + *env.ALBDNS
	}
	c.JSON(http.StatusOK, resp)
}

// HandleScaleService updates the ECS desired count for a specific environment.
func (s *Service) HandleScaleService(c *gin.Context) {
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

	var body struct {
		Replicas int `json:"replicas"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "replicas required"})
		return
	}
	if body.Replicas < 0 || body.Replicas > 10 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "replicas must be between 0 and 10"})
		return
	}

	env, err := s.getEnvironmentByID(c.Request.Context(), envID)
	if err != nil || env.ProjectID != projectID {
		c.JSON(http.StatusNotFound, gin.H{"error": "environment not found"})
		return
	}
	if env.ECSServiceName == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "environment not yet deployed"})
		return
	}

	clusterName, _, _, _, err := s.resolveNetworking(c.Request.Context(), env)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve cluster: " + err.Error()})
		return
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(c.Request.Context(), env)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to connect to AWS: " + err.Error()})
		return
	}

	if err := s.awsSvc.UpdateServiceDesiredCount(c.Request.Context(), clients, clusterName, *env.ECSServiceName, int32(body.Replicas)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Scaling %s to %d replica(s)", env.Name, body.Replicas)})
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

// hasInFlightDeployment reports whether the environment already has a deployment in
// pending/building/deploying state. Guards against concurrent rollouts racing on the
// same ECS service. The watchdog auto-fails rows stuck past stuckThresholdMinutes,
// so a crashed worker cannot block deploys forever.
func (s *Service) hasInFlightDeployment(ctx context.Context, envID uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.Pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM deployments
			WHERE environment_id = $1 AND status IN ('pending', 'building', 'deploying')
		)`, envID,
	).Scan(&exists)
	return exists, err
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
	if s.webhooksSvc != nil {
		s.webhooksSvc.FireEvent(projectID, models.WebhookEventDeployFailed,
			webhooks.BuildPayload(projectID.String(), "", "", deploymentID.String(), "", reason))
	}
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

// ---- Cost Intelligence ----

// HandleGetCosts returns the last-30-day AWS cost breakdown for the project's linked account.
func (s *Service) HandleGetCosts(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	summary, err := s.awsSvc.GetAccountCostSummary(c.Request.Context(), projectID.String())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, summary)
}

// GetCostSummary is called by the conversation service to produce a human-readable cost summary.
func (s *Service) GetCostSummary(ctx context.Context, projectID uuid.UUID) (string, error) {
	summary, err := s.awsSvc.GetAccountCostSummary(ctx, projectID.String())
	if err != nil {
		return "", fmt.Errorf("couldn't retrieve cost data: %w", err)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Your ConvDeploy infrastructure has cost **$%.2f** over the last 30 days:\n\n", summary.TotalMonthlyCost))
	for svc, cost := range summary.ByService {
		if cost > 0.001 {
			sb.WriteString(fmt.Sprintf("- **%s**: $%.2f\n", svc, cost))
		}
	}
	if summary.TotalMonthlyCost < 1 {
		sb.WriteString("\nLooks like costs are very low — everything is running lean.")
	} else if summary.TotalMonthlyCost > 100 {
		sb.WriteString("\nTip: if any environments are idle, scaling to 0 replicas will eliminate compute costs while keeping infrastructure in place.")
	}
	return sb.String(), nil
}

// ---- Conversational Infrastructure Mutations ----

// ProposeResourceChange classifies the requested CPU/memory change, shows the diff, and stores
// a pending mutation keyed by projectID. The user must confirm with "yes" to apply.
func (s *Service) ProposeResourceChange(ctx context.Context, projectID, userID uuid.UUID, cpu, memory string) (string, error) {
	if cpu == "" && memory == "" {
		return "Please specify the CPU and/or memory you'd like — e.g. \"set to 2 vCPU and 4 GB memory\".", nil
	}

	env, err := s.getDeployableEnvironment(ctx, projectID)
	if err != nil {
		return "No ready environment found to update.", nil
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, env)
	if err != nil {
		return "Failed to connect to AWS.", err
	}

	clusterName, _, _, _, err := s.resolveNetworking(ctx, env)
	if err != nil {
		return "Couldn't resolve environment networking.", err
	}
	if env.ECSServiceName == nil {
		return "No ECS service found for this environment.", nil
	}

	currentCPU, currentMem, err := s.awsSvc.GetCurrentTaskResources(ctx, clients, clusterName, *env.ECSServiceName)
	if err != nil {
		currentCPU, currentMem = "unknown", "unknown"
	}

	// Fill defaults if only one dimension was specified.
	if cpu == "" {
		cpu = currentCPU
	}
	if memory == "" {
		memory = currentMem
	}

	proposal := &models.MutationProposal{
		ProjectID:     projectID,
		EnvName:       env.Name,
		CPU:           cpu,
		Memory:        memory,
		CurrentCPU:    currentCPU,
		CurrentMemory: currentMem,
	}

	s.mutationMu.Lock()
	s.pendingMutations[projectID] = &pendingMutation{proposal: proposal, proposedAt: time.Now()}
	s.mutationMu.Unlock()

	return fmt.Sprintf(
		"I'll update **%s** from %s CPU / %s MB → **%s CPU / %s MB**. This triggers a rolling restart (~30s downtime window).\n\nReply **confirm** to apply, or anything else to cancel.",
		env.Name, currentCPU, currentMem, cpu, memory,
	), nil
}

// ApplyPendingMutation executes the most recently proposed infra change for the project.
func (s *Service) ApplyPendingMutation(ctx context.Context, projectID, userID uuid.UUID) (string, error) {
	s.mutationMu.Lock()
	pm, ok := s.pendingMutations[projectID]
	if ok {
		delete(s.pendingMutations, projectID)
	}
	s.mutationMu.Unlock()

	if !ok || time.Since(pm.proposedAt) > 10*time.Minute {
		return "No pending change to confirm (it may have expired). Propose a new change first.", nil
	}

	p := pm.proposal
	env, err := s.getEnvironment(ctx, projectID, p.EnvName)
	if err != nil {
		return "", fmt.Errorf("environment not found: %w", err)
	}

	project, err := s.getProject(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("project not found: %w", err)
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, env)
	if err != nil {
		return "", fmt.Errorf("failed to assume role: %w", err)
	}

	// Use the latest live image.
	var imageURI string
	s.db.Pool.QueryRow(ctx,
		`SELECT image_uri FROM deployments WHERE environment_id = $1 AND status = 'live' ORDER BY created_at DESC LIMIT 1`,
		env.ID,
	).Scan(&imageURI)
	if imageURI == "" {
		return "No live deployment found to base the update on — deploy first.", nil
	}

	if err := s.awsSvc.UpdateServiceResources(ctx, clients, env, project, imageURI, p.CPU, p.Memory); err != nil {
		return "", fmt.Errorf("failed to apply resource change: %w", err)
	}

	return fmt.Sprintf("Done! **%s** is updating to %s CPU / %s MB. The rolling restart will complete in ~30–60 seconds.", p.EnvName, p.CPU, p.Memory), nil
}

// ---- PR Preview Environments ----

// HandleEnablePreviews registers a GitHub webhook on the project repo and stores the
// webhook ID + secret in the project record. Safe to call multiple times (idempotent via delete+re-register).
func (s *Service) HandleEnablePreviews(c *gin.Context) {
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

	token, err := s.githubSvc.GetTokenForDeployment(c.Request.Context(), project.UserID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "GitHub not connected"})
		return
	}

	// Generate a fresh webhook secret.
	secretBytes := make([]byte, 20)
	rand.Read(secretBytes)
	secret := hex.EncodeToString(secretBytes)

	// Prefer the explicitly configured public URL — the Host header is client-controlled
	// and wrong behind proxies. Fall back to request-derived URL for local dev.
	var webhookURL string
	if base := os.Getenv("PUBLIC_API_URL"); base != "" {
		webhookURL = strings.TrimRight(base, "/") + "/api/v1/github/webhook"
	} else {
		proto := c.GetHeader("X-Forwarded-Proto")
		if proto == "" {
			proto = "http"
		}
		webhookURL = proto + "://" + c.Request.Host + "/api/v1/github/webhook"
	}

	// GitHub rejects webhook URLs that aren't reachable over the public Internet.
	// Catch the common local-dev case up front with a clear explanation instead of
	// surfacing GitHub's 422.
	if isNonPublicURL(webhookURL) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf(
				"PR previews need a publicly reachable API URL, but this server is at %s. "+
					"Expose it with a tunnel (e.g. `ngrok http 8080`), set PUBLIC_API_URL to the tunnel URL in .env, restart, and try again.",
				webhookURL,
			),
		})
		return
	}

	// Delete old hook if present.
	if project.GithubWebhookID != nil {
		s.githubSvc.DeleteRepoWebhook(c.Request.Context(), token, project.RepoOwner, project.RepoName, *project.GithubWebhookID)
	}

	hookID, err := s.githubSvc.RegisterRepoWebhook(c.Request.Context(), token, project.RepoOwner, project.RepoName, webhookURL, secret)
	if err != nil {
		if strings.Contains(err.Error(), "reachable over the public Internet") {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf(
					"GitHub can't reach %s from the Internet. Expose the API with a tunnel (e.g. `ngrok http 8080`), set PUBLIC_API_URL to the tunnel URL in .env, restart, and try again.",
					webhookURL,
				),
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register webhook: " + err.Error()})
		return
	}

	_, err = s.db.Pool.Exec(c.Request.Context(),
		`UPDATE projects SET github_webhook_id = $1, github_webhook_secret = $2, updated_at = NOW() WHERE id = $3`,
		hookID, secret, projectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save webhook"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "PR preview environments enabled", "webhook_id": hookID})
}

// HandleDisablePreviews removes the GitHub webhook and clears webhook fields on the project.
func (s *Service) HandleDisablePreviews(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	var webhookID *int64
	var webhookSecret, repoOwner, repoName string
	var userID uuid.UUID
	s.db.Pool.QueryRow(c.Request.Context(),
		`SELECT user_id, repo_owner, repo_name, github_webhook_id, github_webhook_secret FROM projects WHERE id = $1`,
		projectID,
	).Scan(&userID, &repoOwner, &repoName, &webhookID, &webhookSecret)

	if webhookID != nil {
		token, _ := s.githubSvc.GetTokenForDeployment(c.Request.Context(), userID)
		if token != "" {
			s.githubSvc.DeleteRepoWebhook(c.Request.Context(), token, repoOwner, repoName, *webhookID)
		}
	}

	s.db.Pool.Exec(c.Request.Context(),
		`UPDATE projects SET github_webhook_id = NULL, github_webhook_secret = '', updated_at = NOW() WHERE id = $1`,
		projectID,
	)

	c.JSON(http.StatusOK, gin.H{"message": "PR preview environments disabled"})
}

// HandleGithubWebhook receives GitHub pull_request events, verifies the HMAC signature,
// and dispatches to the preview lifecycle handler.
func (s *Service) HandleGithubWebhook(c *gin.Context) {
	event := c.GetHeader("X-GitHub-Event")
	if event == "" {
		c.Status(http.StatusBadRequest)
		return
	}

	body, err := c.GetRawData()
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}

	if event != "pull_request" {
		c.Status(http.StatusNoContent)
		return
	}

	var payload githubsvc.PREvent
	if err := json.Unmarshal(body, &payload); err != nil { //nolint:all
		c.Status(http.StatusBadRequest)
		return
	}

	// Find the project that owns this repo.
	var projectID uuid.UUID
	var webhookSecret string
	var userID uuid.UUID
	err = s.db.Pool.QueryRow(c.Request.Context(),
		`SELECT id, user_id, github_webhook_secret FROM projects
		 WHERE repo_owner = $1 AND repo_name = $2 AND github_webhook_id IS NOT NULL
		 LIMIT 1`,
		payload.Repository.Owner.Login, payload.Repository.Name,
	).Scan(&projectID, &userID, &webhookSecret)
	if err != nil {
		// Unknown repo or previews not enabled — silently accept.
		c.Status(http.StatusNoContent)
		return
	}

	// Verify signature.
	sig := c.GetHeader("X-Hub-Signature-256")
	if !githubsvc.VerifyWebhookSignature(body, sig, webhookSecret) {
		c.Status(http.StatusUnauthorized)
		return
	}

	go s.handlePREvent(context.Background(), projectID, userID, &payload)
	c.Status(http.StatusNoContent)
}

// handlePREvent dispatches PR lifecycle actions in a background goroutine.
func (s *Service) handlePREvent(ctx context.Context, projectID, userID uuid.UUID, event *githubsvc.PREvent) {
	switch event.Action {
	case "opened", "reopened", "synchronize":
		s.handlePROpenedOrSync(ctx, projectID, userID, event)
	case "closed":
		s.handlePRClosed(ctx, projectID, event)
	}
}

// handlePROpenedOrSync creates or updates a preview environment and triggers a deploy.
func (s *Service) handlePROpenedOrSync(ctx context.Context, projectID, userID uuid.UUID, event *githubsvc.PREvent) {
	prNum := event.Number
	prBranch := event.PullRequest.Head.Ref
	prSHA := event.PullRequest.Head.SHA

	project, err := s.getProject(ctx, projectID)
	if err != nil {
		log.Printf("[preview] project %s not found: %v", projectID, err)
		return
	}
	if project.AccountID == nil {
		return // no AWS account linked — skip
	}

	// Need a ready staging env to share infra with.
	stagingEnv, err := s.getEnvironment(ctx, projectID, "staging")
	if err != nil || stagingEnv.StackStatus != models.StackStatusReady {
		log.Printf("[preview] no ready staging env for project %s", projectID)
		return
	}

	// Upsert the preview environment record.
	var previewEnvID uuid.UUID
	err = s.db.Pool.QueryRow(ctx, `
		INSERT INTO environments (project_id, name, aws_region, account_id, platform_stack_id,
			cloudformation_stack_id, stack_status, alb_dns,
			ecr_repo_uri, codebuild_project_name, task_execution_role_arn, log_group_name,
			ecs_security_group_id, vpc_subnets,
			is_preview, pr_number, pr_branch, pr_head_sha)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8, $9, $10, $11, $12, $13, true, $14, $15, $16)
		ON CONFLICT (project_id, pr_number) WHERE is_preview = true
		DO UPDATE SET pr_head_sha = EXCLUDED.pr_head_sha, pr_branch = EXCLUDED.pr_branch,
		              stack_status = 'pending', updated_at = NOW()
		RETURNING id`,
		projectID,
		fmt.Sprintf("pr-%d", prNum),
		stagingEnv.AWSRegion,
		stagingEnv.AccountID,
		stagingEnv.PlatformStackID,
		stagingEnv.CloudFormationStackID,
		stagingEnv.ALBDNS,
		stagingEnv.ECRRepoURI,
		stagingEnv.CodeBuildProjectName,
		stagingEnv.TaskExecutionRoleARN,
		stagingEnv.LogGroupName,
		stagingEnv.ECSSecurityGroupID,
		stagingEnv.VPCSubnets,
		prNum, prBranch, prSHA,
	).Scan(&previewEnvID)
	if err != nil {
		log.Printf("[preview] failed to upsert preview env: %v", err)
		return
	}

	// Post initial PR comment.
	token, _ := s.githubSvc.GetTokenForDeployment(ctx, userID)
	commentBody := fmt.Sprintf("🚀 **ConvDeploy** is building a preview for this PR...\n\n*Commit: `%s`*", prSHA[:8])
	var commentID int64
	if token != "" {
		commentID, _ = s.githubSvc.CreatePRComment(ctx, token, project.RepoOwner, project.RepoName, prNum, commentBody)
		if commentID > 0 {
			s.db.Pool.Exec(ctx, `UPDATE environments SET github_pr_comment_id = $1 WHERE id = $2`, commentID, previewEnvID)
		}
	}

	// Create a deployment record for the PR SHA.
	var depID uuid.UUID
	s.db.Pool.QueryRow(ctx,
		`INSERT INTO deployments (project_id, environment_id, commit_sha, commit_message, status)
		 VALUES ($1, $2, $3, $4, 'pending') RETURNING id`,
		projectID, previewEnvID,
		prSHA,
		fmt.Sprintf("PR #%d: %s", prNum, prBranch),
	).Scan(&depID)
	if depID == uuid.Nil {
		return
	}

	// Run the preview deploy (blocking, called in goroutine already).
	if err := s.runPreviewDeployWorkflow(ctx, projectID, previewEnvID, depID, prSHA); err != nil {
		log.Printf("[preview] deploy failed for PR #%d: %v", prNum, err)
		if token != "" && commentID > 0 {
			s.githubSvc.UpdatePRComment(ctx, token, project.RepoOwner, project.RepoName, commentID,
				fmt.Sprintf("❌ **ConvDeploy** preview build failed for PR #%d.\n\n```\n%s\n```", prNum, err.Error()))
		}
		return
	}

	// Reload preview env to get ALB DNS and listener rule ARN.
	var albDNS *string
	s.db.Pool.QueryRow(ctx, `SELECT alb_dns FROM environments WHERE id = $1`, previewEnvID).Scan(&albDNS)
	previewURL := ""
	if albDNS != nil {
		previewURL = fmt.Sprintf("http://%s/pr-%d/", *albDNS, prNum)
	}

	if token != "" && commentID > 0 {
		body := fmt.Sprintf("✅ **Preview environment ready!**\n\n🔗 [Open Preview](%s)\n\n`%s` · PR #%d",
			previewURL, prSHA[:8], prNum)
		s.githubSvc.UpdatePRComment(ctx, token, project.RepoOwner, project.RepoName, commentID, body)
	}
}

// handlePRClosed tears down the preview environment and updates the PR comment.
func (s *Service) handlePRClosed(ctx context.Context, projectID uuid.UUID, event *githubsvc.PREvent) {
	prNum := event.Number

	var previewEnv models.Environment
	var commentID *int64
	err := s.db.Pool.QueryRow(ctx, `
		SELECT id, project_id, name, aws_region, account_id, platform_stack_id,
		       cloudformation_stack_id, stack_status, alb_dns,
		       ecr_repo_uri, ecs_cluster_name, ecs_service_name,
		       codebuild_project_name, task_execution_role_arn, log_group_name,
		       alb_target_group_arn, alb_listener_rule_arn, ecs_security_group_id, vpc_subnets,
		       github_pr_comment_id, created_at, updated_at
		FROM environments
		WHERE project_id = $1 AND pr_number = $2 AND is_preview = true`,
		projectID, prNum,
	).Scan(
		&previewEnv.ID, &previewEnv.ProjectID, &previewEnv.Name, &previewEnv.AWSRegion,
		&previewEnv.AccountID, &previewEnv.PlatformStackID,
		&previewEnv.CloudFormationStackID, &previewEnv.StackStatus, &previewEnv.ALBDNS,
		&previewEnv.ECRRepoURI, &previewEnv.ECSClusterName, &previewEnv.ECSServiceName,
		&previewEnv.CodeBuildProjectName, &previewEnv.TaskExecutionRoleARN, &previewEnv.LogGroupName,
		&previewEnv.ALBTargetGroupARN, &previewEnv.ALBListenerRuleARN, &previewEnv.ECSSecurityGroupID, &previewEnv.VPCSubnets,
		&commentID, &previewEnv.CreatedAt, &previewEnv.UpdatedAt,
	)
	if err != nil {
		return // no preview env for this PR
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, &previewEnv)
	if err == nil {
		s.awsSvc.TeardownPreviewService(ctx, clients, &previewEnv)
	}

	s.db.Pool.Exec(ctx, `DELETE FROM environments WHERE id = $1`, previewEnv.ID)

	// Update PR comment.
	var userID uuid.UUID
	var repoOwner, repoName string
	s.db.Pool.QueryRow(ctx,
		`SELECT user_id, repo_owner, repo_name FROM projects WHERE id = $1`, projectID,
	).Scan(&userID, &repoOwner, &repoName)

	token, _ := s.githubSvc.GetTokenForDeployment(ctx, userID)
	if token != "" && commentID != nil && *commentID > 0 {
		s.githubSvc.UpdatePRComment(ctx, token, repoOwner, repoName, *commentID,
			fmt.Sprintf("🗑️ Preview environment for PR #%d has been torn down.", prNum))
	}
}

// runPreviewDeployWorkflow builds the PR image and creates the preview ECS service.
// Reuses the staging environment's CodeBuild project and ECR repo.
func (s *Service) runPreviewDeployWorkflow(ctx context.Context, projectID, envID, depID uuid.UUID, commitSHA string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	project, err := s.getProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}

	previewEnv, err := s.getEnvironmentByID(ctx, envID)
	if err != nil {
		return fmt.Errorf("preview env not found: %w", err)
	}

	// Need a ready staging env for the platform stack.
	stagingEnv, err := s.getEnvironment(ctx, projectID, "staging")
	if err != nil {
		return fmt.Errorf("no staging env: %w", err)
	}

	_, _, _, ps, err := s.resolveNetworking(ctx, stagingEnv)
	if err != nil {
		return fmt.Errorf("failed to resolve staging networking: %w", err)
	}
	if ps == nil {
		return fmt.Errorf("staging has no platform stack")
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(ctx, stagingEnv)
	if err != nil {
		return fmt.Errorf("failed to assume role: %w", err)
	}

	token, err := s.githubSvc.GetTokenForDeployment(ctx, project.UserID)
	if err != nil {
		return fmt.Errorf("GitHub not connected: %w", err)
	}

	if previewEnv.ECRRepoURI == nil || previewEnv.CodeBuildProjectName == nil {
		return fmt.Errorf("preview env missing ECR or CodeBuild reference")
	}

	imageURI := fmt.Sprintf("%s:%s", *previewEnv.ECRRepoURI, commitSHA[:8])
	ecrRegistry := strings.SplitN(*previewEnv.ECRRepoURI, "/", 2)[0]

	startCommand := ""
	if project.StartCommand != nil {
		startCommand = *project.StartCommand
	}

	s.updateDeploymentStatus(ctx, depID, models.DeployStatusBuilding, nil, nil)

	buildRes, err := s.awsSvc.StartCodeBuildJob(
		ctx, clients, projectID.String(), *previewEnv.CodeBuildProjectName,
		token, project.RepoOwner, project.RepoName,
		commitSHA, imageURI, ecrRegistry, project.Framework, startCommand,
	)
	if err != nil {
		s.updateDeploymentStatus(ctx, depID, models.DeployStatusFailed, strPtr("build failed: "+err.Error()), nil)
		return fmt.Errorf("build failed: %w", err)
	}

	if buildRes.TokenParamName != "" {
		// Detached context — the delete must succeed even if the workflow context expired.
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.awsSvc.DeleteSSMParameter(cleanupCtx, clients, buildRes.TokenParamName)
		}()
	}

	if err := s.awsSvc.WaitForCodeBuild(ctx, clients, buildRes.BuildID, func(msg string) {
		log.Printf("[preview] build %s: %s", buildRes.BuildID, msg)
	}); err != nil {
		s.updateDeploymentStatus(ctx, depID, models.DeployStatusFailed, strPtr("build failed: "+err.Error()), nil)
		return fmt.Errorf("build failed: %w", err)
	}

	s.updateDeploymentStatus(ctx, depID, models.DeployStatusDeploying, nil, &imageURI)

	// Copy task execution fields from staging to preview env for CreatePreviewService.
	previewEnv.TaskExecutionRoleARN = stagingEnv.TaskExecutionRoleARN
	logGroup := fmt.Sprintf("/convdeploy/pr-%d-%s", *previewEnv.PRNumber, projectID.String()[:8])
	previewEnv.LogGroupName = &logGroup

	// Ensure the CloudWatch log group exists for this preview.
	s.awsSvc.CreateLogGroupIfNotExists(ctx, clients, logGroup)

	ruleARN, tgARN, err := s.awsSvc.CreatePreviewService(ctx, clients, stagingEnv, ps, previewEnv, imageURI, project)
	if err != nil {
		s.updateDeploymentStatus(ctx, depID, models.DeployStatusFailed, strPtr("preview deploy failed: "+err.Error()), nil)
		return fmt.Errorf("preview deploy failed: %w", err)
	}

	// Persist the new resource ARNs.
	s.db.Pool.Exec(ctx, `
		UPDATE environments
		SET stack_status = 'ready', alb_listener_rule_arn = $1, alb_target_group_arn = $2,
		    log_group_name = $3, task_execution_role_arn = $4, updated_at = NOW()
		WHERE id = $5`,
		ruleARN, tgARN, logGroup, stagingEnv.TaskExecutionRoleARN, envID,
	)

	s.updateDeploymentStatus(ctx, depID, models.DeployStatusLive, nil, &imageURI)
	return nil
}

func strPtr(s string) *string { return &s }
