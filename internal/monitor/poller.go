// Package monitor is OpsPilot's continuous monitoring subsystem — the part that
// watches infrastructure between deploys. The Poller checks ECS task health and
// ALB metrics every minute, the LogScanner pattern-matches application logs for
// anomalies every five minutes, and the AlertEngine turns the resulting
// operational events into deduplicated, AI-summarized, user-facing alerts.
package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	awssvc "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/google/uuid"
)

const (
	pollInterval        = 60 * time.Second
	workerSyncInterval  = 60 * time.Second
	errRateWarnPercent  = 5.0
	errRateErrorPercent = 20.0
	latencyWarnMs       = 2000.0
)

// envHealthState is the previous poll result for one environment, used to detect
// state transitions (degraded → recovered) and avoid re-emitting the same
// condition every cycle.
type envHealthState struct {
	degraded      bool // running < desired (or down)
	down          bool
	highErrorRate bool
	highLatency   bool
}

// Poller continuously checks ECS service health and ALB metrics for every ready
// environment. It runs a supervisor loop that re-syncs one worker goroutine per
// environment, so environments created after startup are picked up automatically.
type Poller struct {
	db       *models.DB
	awsSvc   *awssvc.Service
	eventSvc *events.Service
	hub      *ws.Hub

	mu      sync.Mutex
	state   map[uuid.UUID]*envHealthState
	workers map[uuid.UUID]context.CancelFunc
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewPoller(db *models.DB, awsSvc *awssvc.Service, eventSvc *events.Service, hub *ws.Hub) *Poller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Poller{
		db:       db,
		awsSvc:   awsSvc,
		eventSvc: eventSvc,
		hub:      hub,
		state:    make(map[uuid.UUID]*envHealthState),
		workers:  make(map[uuid.UUID]context.CancelFunc),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start blocks, supervising one polling goroutine per ready environment.
// Call as `go poller.Start()`.
func (p *Poller) Start() {
	p.syncWorkers()
	ticker := time.NewTicker(workerSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.syncWorkers()
		}
	}
}

// Stop cancels the supervisor and all environment workers.
func (p *Poller) Stop() {
	p.cancel()
}

// monitoredEnv is a single target a monitor worker watches. It is either an OpsPilot
// environment (discovered=false; runtime refs loaded from the environments table) or a
// discovered ECS service assigned to a project (discovered=true; cluster/service/region/
// account carried inline from discovered_resources).
type monitoredEnv struct {
	id        uuid.UUID // environment id, or discovered_resources id when discovered
	projectID uuid.UUID
	name      string

	discovered  bool
	accountID   *uuid.UUID
	region      string
	clusterName string
	serviceName string
	logGroup    string
}

// readyEnvironments lists monitoring targets: ready non-preview environments plus
// discovered ECS services that a user has assigned to a project (so alerts have a home).
func (p *Poller) readyEnvironments(ctx context.Context) ([]monitoredEnv, error) {
	var envs []monitoredEnv

	rows, err := p.db.Pool.Query(ctx,
		`SELECT id, project_id, name FROM environments
		 WHERE stack_status = 'ready' AND is_preview = false`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e monitoredEnv
		if err := rows.Scan(&e.id, &e.projectID, &e.name); err == nil {
			envs = append(envs, e)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Discovered ECS services assigned to a project.
	drows, err := p.db.Pool.Query(ctx,
		`SELECT id, project_id, resource_name, aws_account_id, region,
		        COALESCE(metadata->>'cluster_name',''), COALESCE(metadata->>'service_name','')
		 FROM discovered_resources
		 WHERE resource_type = 'ecs_service' AND project_id IS NOT NULL`)
	if err != nil {
		return envs, nil // environments still monitored even if discovery query fails
	}
	defer drows.Close()
	for drows.Next() {
		var e monitoredEnv
		var acct uuid.UUID
		if err := drows.Scan(&e.id, &e.projectID, &e.name, &acct, &e.region, &e.clusterName, &e.serviceName); err != nil {
			continue
		}
		if e.clusterName == "" || e.serviceName == "" {
			continue
		}
		e.discovered = true
		e.accountID = &acct
		envs = append(envs, e)
	}
	return envs, nil
}

// syncWorkers reconciles running workers with the current set of ready
// environments — starting workers for new environments and stopping workers for
// environments that were deleted or are no longer ready.
func (p *Poller) syncWorkers() {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	envs, err := p.readyEnvironments(ctx)
	if err != nil {
		slog.Error("poller: failed to list environments", "component", "monitor.poller", "error", err)
		return
	}

	current := make(map[uuid.UUID]monitoredEnv, len(envs))
	for _, e := range envs {
		current[e.id] = e
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for id, stop := range p.workers {
		if _, ok := current[id]; !ok {
			stop()
			delete(p.workers, id)
			delete(p.state, id)
		}
	}
	for id, env := range current {
		if _, ok := p.workers[id]; ok {
			continue
		}
		wctx, wcancel := context.WithCancel(p.ctx)
		p.workers[id] = wcancel
		go p.runWorker(wctx, env)
	}
}

// runWorker polls one environment until cancelled. Each cycle is wrapped in
// recover() so a panic in one environment never affects the others.
func (p *Poller) runWorker(ctx context.Context, env monitoredEnv) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.safePoll(ctx, env)
		}
	}
}

func (p *Poller) safePoll(ctx context.Context, env monitoredEnv) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("poller: panic recovered", "component", "monitor.poller",
				"environment_id", env.id, "panic", r)
		}
	}()
	if err := p.pollOnce(ctx, env); err != nil {
		slog.Warn("poller: poll failed", "component", "monitor.poller",
			"project_id", env.projectID, "environment_id", env.id, "error", err)
	}
}

// pollOnce performs one health check cycle for an environment.
func (p *Poller) pollOnce(ctx context.Context, env monitoredEnv) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// Discovered ECS service: assume the account role for its region and check health
	// directly. No ALB metrics (we don't know the load balancer), so only task
	// up/degraded/recovered transitions are evaluated.
	if env.discovered {
		if env.accountID == nil || env.clusterName == "" || env.serviceName == "" {
			return nil
		}
		clients, err := p.awsSvc.AssumeRoleForAccountAndRegion(ctx, *env.accountID, env.region)
		if err != nil {
			return err
		}
		health, err := p.awsSvc.DescribeECSService(ctx, clients, env.clusterName, env.serviceName)
		if err != nil {
			return err
		}
		p.evaluate(ctx, env, health, &awssvc.ALBMetrics{})
		return nil
	}

	// Load runtime references (cluster from platform stack, ALB/TG ARNs).
	var (
		ecsService, clusterName *string
		albArn, tgArn           *string
	)
	err := p.db.Pool.QueryRow(ctx, `
		SELECT e.ecs_service_name,
		       COALESCE(ps.ecs_cluster_name, e.ecs_cluster_name),
		       ps.alb_arn, e.alb_target_group_arn
		FROM environments e
		LEFT JOIN platform_stacks ps ON ps.id = e.platform_stack_id
		WHERE e.id = $1`, env.id,
	).Scan(&ecsService, &clusterName, &albArn, &tgArn)
	if err != nil {
		return err
	}
	if ecsService == nil || clusterName == nil {
		return nil // not deployed yet — nothing to monitor
	}

	fullEnv, err := p.loadEnv(ctx, env.id)
	if err != nil {
		return err
	}
	clients, err := p.awsSvc.AssumeRoleForEnvironment(ctx, fullEnv)
	if err != nil {
		return err
	}

	health, err := p.awsSvc.DescribeECSService(ctx, clients, *clusterName, *ecsService)
	if err != nil {
		return err
	}

	metrics := &awssvc.ALBMetrics{}
	if albArn != nil {
		if m, err := p.awsSvc.GetALBMetrics(ctx, clients, *albArn, derefStr(tgArn)); err == nil {
			metrics = m
		}
	}

	p.evaluate(ctx, env, health, metrics)
	return nil
}

// evaluate compares this poll against the previous state and emits operational
// events on condition transitions (not every cycle, to avoid event spam — the
// alert engine additionally dedups at the alert level).
func (p *Poller) evaluate(ctx context.Context, env monitoredEnv, h *awssvc.ServiceHealth, m *awssvc.ALBMetrics) {
	p.mu.Lock()
	prev, ok := p.state[env.id]
	if !ok {
		prev = &envHealthState{}
		p.state[env.id] = prev
	}
	was := *prev
	p.mu.Unlock()

	now := envHealthState{}
	now.down = h.RunningCount == 0 && h.DesiredCount > 0
	now.degraded = h.RunningCount < h.DesiredCount

	errRatePct := 0.0
	if m.RequestCount > 0 {
		errRatePct = m.Request5xxCount / m.RequestCount * 100
	}
	now.highErrorRate = errRatePct > errRateWarnPercent
	now.highLatency = m.P99LatencyMs > latencyWarnMs

	envID := env.id
	emit := func(eventType, severity string, payload map[string]any) {
		p.eventSvc.Emit(ctx, events.Event{
			ProjectID:     env.projectID,
			EnvironmentID: &envID,
			Type:          eventType,
			Severity:      severity,
			Source:        models.SourceScheduler,
			Payload:       payload,
		})
		if severity != models.SeverityInfo {
			p.hub.Broadcast(env.projectID.String(), ws.Message{
				Type:    "runtime_event",
				Payload: eventType + ": " + env.name,
			})
		}
	}

	switch {
	case now.down && !was.down:
		emit(models.EventRuntimeServiceDown, models.SeverityError, map[string]any{
			"env_name": env.name, "running": h.RunningCount, "desired": h.DesiredCount,
		})
	case now.degraded && !was.degraded && !now.down:
		emit(models.EventRuntimeTasksDegraded, models.SeverityWarn, map[string]any{
			"env_name": env.name, "running": h.RunningCount, "desired": h.DesiredCount,
		})
	}

	if now.highErrorRate && !was.highErrorRate {
		severity := models.SeverityWarn
		if errRatePct > errRateErrorPercent {
			severity = models.SeverityError
		}
		emit(models.EventRuntimeHighErrorRate, severity, map[string]any{
			"env_name": env.name, "error_rate_pct": errRatePct,
			"requests_5xx": m.Request5xxCount, "requests_total": m.RequestCount,
		})
	}

	if now.highLatency && !was.highLatency {
		emit(models.EventRuntimeHighLatency, models.SeverityWarn, map[string]any{
			"env_name": env.name, "p99_latency_ms": m.P99LatencyMs,
		})
	}

	// Recovery: previously in any bad state, now fully healthy.
	wasBad := was.down || was.degraded || was.highErrorRate
	nowGood := !now.down && !now.degraded && !now.highErrorRate
	if wasBad && nowGood && h.DesiredCount > 0 {
		emit(models.EventRuntimeServiceRecovered, models.SeverityInfo, map[string]any{
			"env_name": env.name, "running": h.RunningCount, "desired": h.DesiredCount,
		})
	}

	p.mu.Lock()
	p.state[env.id] = &now
	p.mu.Unlock()
}

// loadEnv fetches the fields AssumeRoleForEnvironment needs.
func (p *Poller) loadEnv(ctx context.Context, envID uuid.UUID) (*models.Environment, error) {
	var env models.Environment
	err := p.db.Pool.QueryRow(ctx,
		`SELECT id, project_id, account_id, aws_region FROM environments WHERE id = $1`, envID,
	).Scan(&env.ID, &env.ProjectID, &env.AccountID, &env.AWSRegion)
	if err != nil {
		return nil, err
	}
	return &env, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
