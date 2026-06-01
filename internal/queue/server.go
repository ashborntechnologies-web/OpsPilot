package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ashborntechnologies-web/OpsPilot/internal/deploy"
	"github.com/ashborntechnologies-web/OpsPilot/internal/diagnosis"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

const (
	TaskDeploy    = "deploy:run"
	TaskDiagnose  = "diagnosis:run"
	TaskProvision = "provision:run"
	TaskRollback  = "rollback:run"
)

type DeployPayload struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	DeploymentID  string `json:"deployment_id"`
	CommitSHA     string `json:"commit_sha"`
}

type RollbackPayload struct {
	ProjectID            string `json:"project_id"`
	EnvironmentID        string `json:"environment_id"`
	DeploymentID         string `json:"deployment_id"`          // the new rollback deployment record
	PreviousDeploymentID string `json:"previous_deployment_id"` // the deployment being rolled back from
}

type DiagnosePayload struct {
	ProjectID    string `json:"project_id"`
	DeploymentID string `json:"deployment_id"`
}

type ProvisionPayload struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
}

// Server runs Asynq workers that process background jobs.
type Server struct {
	server       *asynq.Server
	mux          *asynq.ServeMux
	deploySvc    *deploy.Service
	diagnosisSvc *diagnosis.Service
}

func NewServer(redisURL string, deploySvc *deploy.Service, diagnosisSvc *diagnosis.Service) *Server {
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: redisURL},
		asynq.Config{
			Concurrency: 5,
			Queues: map[string]int{
				"critical": 6, // deploys
				"default":  3, // diagnosis, other tasks
			},
		},
	)

	s := &Server{
		server:       srv,
		mux:          asynq.NewServeMux(),
		deploySvc:    deploySvc,
		diagnosisSvc: diagnosisSvc,
	}

	s.mux.HandleFunc(TaskDeploy, s.handleDeploy)
	s.mux.HandleFunc(TaskDiagnose, s.handleDiagnose)
	s.mux.HandleFunc(TaskProvision, s.handleProvision)
	s.mux.HandleFunc(TaskRollback, s.handleRollback)

	return s
}

// Start runs the Asynq worker loop. It blocks until the server stops. On error it
// logs rather than calling log.Fatal so a transient worker failure does not take down
// the HTTP API running in the same process.
func (s *Server) Start() {
	if err := s.server.Run(s.mux); err != nil {
		log.Printf("Asynq server stopped with error: %v", err)
	}
}

func (s *Server) Stop() {
	s.server.Shutdown()
}

// handleDeploy is the Asynq task handler for deploy jobs.
// It delegates the full workflow to deploy.Service.RunDeployWorkflow.
func (s *Server) handleDeploy(ctx context.Context, t *asynq.Task) error {
	var p DeployPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("failed to unmarshal deploy payload: %w", err)
	}

	projectID, err := uuid.Parse(p.ProjectID)
	if err != nil {
		return fmt.Errorf("invalid project ID: %w", err)
	}

	environmentID, err := uuid.Parse(p.EnvironmentID)
	if err != nil {
		return fmt.Errorf("invalid environment ID: %w", err)
	}

	deploymentID, err := uuid.Parse(p.DeploymentID)
	if err != nil {
		return fmt.Errorf("invalid deployment ID: %w", err)
	}

	log.Printf("[deploy] starting job project=%s env=%s deploy=%s commit=%s",
		p.ProjectID[:8], p.EnvironmentID[:8], p.DeploymentID[:8], p.CommitSHA[:8])

	return s.deploySvc.RunDeployWorkflow(ctx, projectID, environmentID, deploymentID)
}

func (s *Server) handleProvision(ctx context.Context, t *asynq.Task) error {
	var p ProvisionPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("%w: unmarshal provision payload: %w", asynq.SkipRetry, err)
	}

	projectID, err := uuid.Parse(p.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project ID: %w", asynq.SkipRetry, err)
	}

	environmentID, err := uuid.Parse(p.EnvironmentID)
	if err != nil {
		return fmt.Errorf("%w: invalid environment ID: %w", asynq.SkipRetry, err)
	}

	log.Printf("[provision] starting job project=%s env=%s", p.ProjectID[:8], p.EnvironmentID[:8])

	if err := s.deploySvc.RunProvisionWorkflow(ctx, projectID, environmentID); err != nil {
		log.Printf("[provision] FAILED project=%s env=%s error=%v", p.ProjectID[:8], p.EnvironmentID[:8], err)
		// Never auto-retry provision jobs — the env is marked failed in the DB.
		// The user retries manually via the UI "Retry" button.
		return fmt.Errorf("%w: %w", asynq.SkipRetry, err)
	}

	log.Printf("[provision] completed project=%s env=%s", p.ProjectID[:8], p.EnvironmentID[:8])
	return nil
}

// handleRollback re-deploys a previously-built image to the ECS service. It never
// rebuilds, so it does not auto-retry (a rollback that fails is surfaced to the user).
func (s *Server) handleRollback(ctx context.Context, t *asynq.Task) error {
	var p RollbackPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("%w: unmarshal rollback payload: %w", asynq.SkipRetry, err)
	}

	projectID, err := uuid.Parse(p.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project ID: %w", asynq.SkipRetry, err)
	}
	environmentID, err := uuid.Parse(p.EnvironmentID)
	if err != nil {
		return fmt.Errorf("%w: invalid environment ID: %w", asynq.SkipRetry, err)
	}
	deploymentID, err := uuid.Parse(p.DeploymentID)
	if err != nil {
		return fmt.Errorf("%w: invalid deployment ID: %w", asynq.SkipRetry, err)
	}
	var previousID *uuid.UUID
	if p.PreviousDeploymentID != "" {
		if pid, perr := uuid.Parse(p.PreviousDeploymentID); perr == nil {
			previousID = &pid
		}
	}

	log.Printf("[rollback] starting job project=%s env=%s deploy=%s", p.ProjectID[:8], p.EnvironmentID[:8], p.DeploymentID[:8])

	if err := s.deploySvc.RunRollbackWorkflow(ctx, projectID, environmentID, deploymentID, previousID); err != nil {
		return fmt.Errorf("%w: %w", asynq.SkipRetry, err)
	}
	return nil
}

func (s *Server) handleDiagnose(ctx context.Context, t *asynq.Task) error {
	var p DiagnosePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("failed to unmarshal diagnose payload: %w", err)
	}

	projectID, err := uuid.Parse(p.ProjectID)
	if err != nil {
		return fmt.Errorf("invalid project ID: %w", err)
	}

	_, err = s.diagnosisSvc.DiagnoseProject(ctx, projectID)
	return err
}

// ---- task constructors ----

func NewProvisionTask(projectID, environmentID string) (*asynq.Task, error) {
	payload, err := json.Marshal(ProvisionPayload{
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TaskProvision, payload, asynq.Queue("critical"), asynq.MaxRetry(0)), nil
}

func NewDeployTask(projectID, environmentID, deploymentID, commitSHA string) (*asynq.Task, error) {
	payload, err := json.Marshal(DeployPayload{
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deploymentID,
		CommitSHA:     commitSHA,
	})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TaskDeploy, payload, asynq.Queue("critical")), nil
}

func NewRollbackTask(projectID, environmentID, deploymentID, previousDeploymentID string) (*asynq.Task, error) {
	payload, err := json.Marshal(RollbackPayload{
		ProjectID:            projectID,
		EnvironmentID:        environmentID,
		DeploymentID:         deploymentID,
		PreviousDeploymentID: previousDeploymentID,
	})
	if err != nil {
		return nil, err
	}
	// Rollbacks must not auto-retry — they re-deploy an existing image and any
	// failure (e.g. infra drift) needs human attention, not a silent retry loop.
	return asynq.NewTask(TaskRollback, payload, asynq.Queue("critical"), asynq.MaxRetry(0)), nil
}

func NewDiagnoseTask(projectID, deploymentID string) (*asynq.Task, error) {
	payload, err := json.Marshal(DiagnosePayload{
		ProjectID:    projectID,
		DeploymentID: deploymentID,
	})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TaskDiagnose, payload, asynq.Queue("default")), nil
}

// ---- Client — used by other services to enqueue jobs ----

// Client wraps the Asynq client and implements deploy.Enqueuer.
// It is intentionally separate from Server to avoid the deploy ↔ queue circular import.
type Client struct {
	c *asynq.Client
}

func NewClient(redisURL string) *Client {
	return &Client{c: asynq.NewClient(asynq.RedisClientOpt{Addr: redisURL})}
}

func (c *Client) Close() {
	c.c.Close()
}

// EnqueueDeploy implements deploy.Enqueuer.
func (c *Client) EnqueueDeploy(projectID, environmentID, deploymentID, commitSHA string) error {
	task, err := NewDeployTask(projectID, environmentID, deploymentID, commitSHA)
	if err != nil {
		return err
	}
	_, err = c.c.Enqueue(task)
	return err
}

// EnqueueProvision implements deploy.Enqueuer.
func (c *Client) EnqueueProvision(projectID, environmentID string) error {
	task, err := NewProvisionTask(projectID, environmentID)
	if err != nil {
		return err
	}
	_, err = c.c.Enqueue(task)
	return err
}

// EnqueueRollback implements deploy.Enqueuer.
func (c *Client) EnqueueRollback(projectID, environmentID, deploymentID, previousDeploymentID string) error {
	task, err := NewRollbackTask(projectID, environmentID, deploymentID, previousDeploymentID)
	if err != nil {
		return err
	}
	_, err = c.c.Enqueue(task)
	return err
}
