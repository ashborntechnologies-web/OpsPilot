package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/deploy"
	"github.com/ashborntechnologies-web/OpsPilot/internal/diagnosis"
	"github.com/ashborntechnologies-web/OpsPilot/internal/discovery"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const (
	TaskDeploy        = "deploy:run"
	TaskDiagnose      = "diagnosis:run"
	TaskProvision     = "provision:run"
	TaskRollback      = "rollback:run"
	TaskWatchdog      = "watchdog:run"
	TaskDeleteProject = "project:delete"
	TaskScan          = "discovery:scan"     // scan one AWS account
	TaskScanAll       = "discovery:scan_all" // enqueue a scan for every account (daily)
)

type DeployPayload struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	DeploymentID  string `json:"deployment_id"`
	CommitSHA     string `json:"commit_sha"`
}

// DeleteProjectPayload and DeleteEnvCleanupInfo live in the deploy package to avoid a
// circular import (queue imports deploy). They are aliased here for readability.
type (
	DeleteProjectPayload = deploy.DeleteProjectPayload
	DeleteEnvCleanupInfo = deploy.DeleteEnvCleanupInfo
)

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

type ScanPayload struct {
	AccountID string `json:"account_id"`
}

// Server runs Asynq workers that process background jobs.
type Server struct {
	server       *asynq.Server
	mux          *asynq.ServeMux
	deploySvc    *deploy.Service
	diagnosisSvc *diagnosis.Service
	discoverySvc *discovery.Service
}

func NewServer(redisURL string, deploySvc *deploy.Service, diagnosisSvc *diagnosis.Service, discoverySvc *discovery.Service) *Server {
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
		discoverySvc: discoverySvc,
	}

	s.mux.HandleFunc(TaskDeploy, s.handleDeploy)
	s.mux.HandleFunc(TaskDiagnose, s.handleDiagnose)
	s.mux.HandleFunc(TaskProvision, s.handleProvision)
	s.mux.HandleFunc(TaskRollback, s.handleRollback)
	s.mux.HandleFunc(TaskWatchdog, s.handleWatchdog)
	s.mux.HandleFunc(TaskDeleteProject, s.handleDeleteProject)
	s.mux.HandleFunc(TaskScan, s.handleScan)
	s.mux.HandleFunc(TaskScanAll, s.handleScanAll)

	return s
}

// Start runs the Asynq worker loop. It blocks until the server stops. On error it
// logs rather than calling log.Fatal so a transient worker failure does not take down
// the HTTP API running in the same process.
func (s *Server) Start() {
	if err := s.server.Run(s.mux); err != nil {
		slog.Error(fmt.Sprintf("Asynq server stopped with error: %v", err))
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

	slog.Info(fmt.Sprintf("[deploy] starting job project=%s env=%s deploy=%s commit=%s",
		p.ProjectID[:8], p.EnvironmentID[:8], p.DeploymentID[:8], p.CommitSHA[:8]))

	if err := s.deploySvc.RunDeployWorkflow(ctx, projectID, environmentID, deploymentID); err != nil {
		// Never auto-retry deploys — the workflow already marked the deployment
		// failed and notified the user; a retry would launch a duplicate CodeBuild
		// job and ECS rollout for a deployment the user believes is dead.
		return fmt.Errorf("%w: %w", asynq.SkipRetry, err)
	}
	return nil
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

	slog.Info(fmt.Sprintf("[provision] starting job project=%s env=%s", p.ProjectID[:8], p.EnvironmentID[:8]))

	if err := s.deploySvc.RunProvisionWorkflow(ctx, projectID, environmentID); err != nil {
		slog.Error(fmt.Sprintf("[provision] FAILED project=%s env=%s error=%v", p.ProjectID[:8], p.EnvironmentID[:8], err))
		// Never auto-retry provision jobs — the env is marked failed in the DB.
		// The user retries manually via the UI "Retry" button.
		return fmt.Errorf("%w: %w", asynq.SkipRetry, err)
	}

	slog.Info(fmt.Sprintf("[provision] completed project=%s env=%s", p.ProjectID[:8], p.EnvironmentID[:8]))
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

	slog.Info(fmt.Sprintf("[rollback] starting job project=%s env=%s deploy=%s", p.ProjectID[:8], p.EnvironmentID[:8], p.DeploymentID[:8]))

	if err := s.deploySvc.RunRollbackWorkflow(ctx, projectID, environmentID, deploymentID, previousID); err != nil {
		return fmt.Errorf("%w: %w", asynq.SkipRetry, err)
	}
	return nil
}

// handleDeleteProject cleans up all AWS resources for a deleted project. The DB record is
// already gone by the time this runs; all info needed is in the payload. Each step is
// best-effort — a failure in one environment does not block the others.
func (s *Server) handleDeleteProject(ctx context.Context, t *asynq.Task) error {
	var p deploy.DeleteProjectPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("%w: unmarshal delete-project payload: %w", asynq.SkipRetry, err)
	}
	slog.Info(fmt.Sprintf("[delete-project] starting cleanup for %q (%d environment(s))", p.ProjectName, len(p.Environments)))
	return s.deploySvc.RunDeleteProjectCleanup(ctx, p)
}

// handleWatchdog runs the stuck-resource reconciler. Enqueued periodically by the Scheduler.
func (s *Server) handleWatchdog(ctx context.Context, _ *asynq.Task) error {
	return s.deploySvc.ReconcileStuckResources(ctx)
}

// handleScan runs a full discovery scan of one AWS account.
func (s *Server) handleScan(ctx context.Context, t *asynq.Task) error {
	var p ScanPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("%w: unmarshal scan payload: %w", asynq.SkipRetry, err)
	}
	accountID, err := uuid.Parse(p.AccountID)
	if err != nil {
		return fmt.Errorf("%w: invalid account ID: %w", asynq.SkipRetry, err)
	}
	slog.Info(fmt.Sprintf("[discovery] scanning account=%s", p.AccountID[:8]))
	return s.discoverySvc.ScanAccountByID(ctx, accountID)
}

// handleScanAll fans out a per-account scan for every connected account. Enqueued daily
// by the Scheduler.
func (s *Server) handleScanAll(ctx context.Context, _ *asynq.Task) error {
	return s.discoverySvc.ScanAllAccounts(ctx)
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

	if p.DeploymentID == "" {
		// Autonomous runtime diagnosis — no specific deployment failed.
		_, err = s.diagnosisSvc.DiagnoseRuntime(ctx, projectID)
	} else {
		_, err = s.diagnosisSvc.DiagnoseProject(ctx, projectID)
	}
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
	// MaxRetry(0): deploy failures are terminal (see handleDeploy) — the user
	// redeploys explicitly.
	return asynq.NewTask(TaskDeploy, payload, asynq.Queue("critical"), asynq.MaxRetry(0)), nil
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

func NewDeleteProjectTask(p deploy.DeleteProjectPayload) (*asynq.Task, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	// No retry — cleanup is best-effort and idempotent per step; a second attempt after a
	// partial failure is unlikely to help without operator intervention.
	return asynq.NewTask(TaskDeleteProject, payload, asynq.Queue("default"), asynq.MaxRetry(0)), nil
}

func NewWatchdogTask() *asynq.Task {
	// Idempotent reconciler — never retry; the next scheduled tick covers any miss.
	return asynq.NewTask(TaskWatchdog, nil, asynq.Queue("default"), asynq.MaxRetry(0))
}

func NewScanTask(accountID string) (*asynq.Task, error) {
	payload, err := json.Marshal(ScanPayload{AccountID: accountID})
	if err != nil {
		return nil, err
	}
	// One retry — a scan is idempotent (upserts), so a transient AWS/throttle error
	// is worth retrying once, but not looping.
	return asynq.NewTask(TaskScan, payload, asynq.Queue("default"), asynq.MaxRetry(1)), nil
}

func NewScanAllTask() *asynq.Task {
	// Fan-out task — idempotent; the next daily tick covers any miss.
	return asynq.NewTask(TaskScanAll, nil, asynq.Queue("default"), asynq.MaxRetry(0))
}

func NewDiagnoseTask(projectID, deploymentID string) (*asynq.Task, error) {
	payload, err := json.Marshal(DiagnosePayload{
		ProjectID:    projectID,
		DeploymentID: deploymentID,
	})
	if err != nil {
		return nil, err
	}
	// Diagnosis hits the Claude API — cap retries so a persistent failure
	// doesn't burn tokens for hours.
	return asynq.NewTask(TaskDiagnose, payload, asynq.Queue("default"), asynq.MaxRetry(2)), nil
}

// ---- Scheduler — enqueues periodic tasks (stuck-resource watchdog) ----

// Scheduler wraps asynq.Scheduler so the periodic watchdog can be started from main
// without leaking the asynq dependency. It enqueues a watchdog task every 5 minutes; the
// Server's handleWatchdog processes it.
type Scheduler struct {
	s *asynq.Scheduler
}

func NewScheduler(redisURL string) *Scheduler {
	return &Scheduler{s: asynq.NewScheduler(asynq.RedisClientOpt{Addr: redisURL}, nil)}
}

// Start registers the periodic schedules (stuck-resource watchdog every 5m; discovery
// scan-all every 24h) and starts the scheduler in the background.
func (sc *Scheduler) Start() error {
	if _, err := sc.s.Register("@every 5m", NewWatchdogTask()); err != nil {
		return err
	}
	if _, err := sc.s.Register("@every 24h", NewScanAllTask()); err != nil {
		return err
	}
	return sc.s.Start()
}

func (sc *Scheduler) Stop() {
	sc.s.Shutdown()
}

// ---- Client — used by other services to enqueue jobs ----

// Client wraps the Asynq client and implements deploy.Enqueuer.
// It is intentionally separate from Server to avoid the deploy ↔ queue circular import.
type Client struct {
	c *asynq.Client
	// rdb is a direct Redis connection used for the pending-mutation store
	// (chat-proposed infra changes awaiting confirmation) so proposals survive
	// process restarts and work across replicas.
	rdb *redis.Client
}

func NewClient(redisURL string) *Client {
	return &Client{
		c:   asynq.NewClient(asynq.RedisClientOpt{Addr: redisURL}),
		rdb: redis.NewClient(&redis.Options{Addr: redisURL}),
	}
}

// pendingMutationKey is the Redis key for a project's proposed infra change.
func pendingMutationKey(projectID string) string { return "mutation:" + projectID }

// SetPendingMutation stores a chat-proposed infra change with a TTL.
func (c *Client) SetPendingMutation(ctx context.Context, projectID string, proposal models.MutationProposal, ttl time.Duration) error {
	b, err := json.Marshal(proposal)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, pendingMutationKey(projectID), b, ttl).Err()
}

// GetPendingMutation returns the stored proposal, or nil when none exists.
func (c *Client) GetPendingMutation(ctx context.Context, projectID string) (*models.MutationProposal, error) {
	b, err := c.rdb.Get(ctx, pendingMutationKey(projectID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p models.MutationProposal
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// DeletePendingMutation removes the stored proposal.
func (c *Client) DeletePendingMutation(ctx context.Context, projectID string) error {
	return c.rdb.Del(ctx, pendingMutationKey(projectID)).Err()
}

func (c *Client) Close() {
	c.c.Close()
	c.rdb.Close()
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

// EnqueueDeleteProject implements deploy.Enqueuer.
func (c *Client) EnqueueDeleteProject(p deploy.DeleteProjectPayload) error {
	task, err := NewDeleteProjectTask(p)
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

// EnqueueScan implements discovery.ScanEnqueuer — enqueues a scan of one AWS account
// and returns the Asynq job ID.
func (c *Client) EnqueueScan(accountID string) (string, error) {
	task, err := NewScanTask(accountID)
	if err != nil {
		return "", err
	}
	info, err := c.c.Enqueue(task)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

// EnqueueDiagnose implements events.DiagnosisEnqueuer. Empty deploymentID means
// "diagnose the current runtime state".
func (c *Client) EnqueueDiagnose(projectID, deploymentID string) error {
	task, err := NewDiagnoseTask(projectID, deploymentID)
	if err != nil {
		return err
	}
	_, err = c.c.Enqueue(task)
	return err
}
