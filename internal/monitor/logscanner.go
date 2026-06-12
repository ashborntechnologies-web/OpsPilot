// logscanner.go detects application-level anomalies (exceptions, OOM kills,
// crash loops, connection failures) by pattern-matching recent CloudWatch logs.
// Detection is pure Go — no AI calls — so it is cheap enough to run every five
// minutes on every environment; AI is only involved later, when a resulting
// alert is summarized or an autonomous diagnosis runs.
package monitor

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	awssvc "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/events"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/google/uuid"
)

const (
	scanInterval     = 5 * time.Minute
	scanLogLines     = 300
	anomalyDedupSpan = 30 * time.Minute
)

// Log anomaly pattern type identifiers (stored in event payloads and used by
// the alert engine to distinguish crash loops from generic anomalies).
const (
	PatternException         = "exception"
	PatternOOM               = "oom"
	PatternCrashLoop         = "crash_loop"
	PatternConnectionFailure = "connection_failure"
	PatternDependencyFailure = "dependency_failure"
)

// anomalyPattern is one detection rule evaluated against recent log lines.
type anomalyPattern struct {
	Type     string
	Severity string
	markers  []string
}

var anomalyPatterns = []anomalyPattern{
	{Type: PatternOOM, Severity: models.SeverityError,
		markers: []string{"out of memory", "oomkilled", "cannot allocate memory", "killed"}},
	{Type: PatternException, Severity: models.SeverityWarn,
		markers: []string{"exception", "traceback", "panic:", "fatal", "fatal error"}},
	{Type: PatternConnectionFailure, Severity: models.SeverityWarn,
		markers: []string{"connection refused", "connection timeout", "econnrefused", "dial tcp"}},
	{Type: PatternDependencyFailure, Severity: models.SeverityWarn,
		markers: []string{"failed to connect", "no such host", "name resolution failed"}},
}

// anomalyMatch is the result of scanning one environment's logs.
type anomalyMatch struct {
	PatternType  string
	Severity     string
	MatchedLines []string
	LineCount    int
}

// ScanLines runs all detection rules against the given log lines. Exported for
// testing; contains no I/O.
func ScanLines(lines []string) []anomalyMatch {
	var matches []anomalyMatch

	for _, p := range anomalyPatterns {
		var matched []string
		for _, line := range lines {
			lower := strings.ToLower(line)
			for _, m := range p.markers {
				if strings.Contains(lower, m) {
					matched = append(matched, line)
					break
				}
			}
		}
		if len(matched) > 0 {
			sample := matched
			if len(sample) > 5 {
				sample = sample[len(sample)-5:]
			}
			matches = append(matches, anomalyMatch{
				PatternType:  p.Type,
				Severity:     p.Severity,
				MatchedLines: sample,
				LineCount:    len(matched),
			})
		}
	}

	// Crash loop: the same error line repeating 3+ times within the last 100 lines.
	tail := lines
	if len(tail) > 100 {
		tail = tail[len(tail)-100:]
	}
	counts := make(map[string]int)
	for _, line := range tail {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "exception") ||
			strings.Contains(lower, "panic") || strings.Contains(lower, "fatal") {
			key := strings.TrimSpace(line)
			if len(key) > 200 {
				key = key[:200]
			}
			counts[key]++
		}
	}
	for line, n := range counts {
		if n >= 3 {
			matches = append(matches, anomalyMatch{
				PatternType:  PatternCrashLoop,
				Severity:     models.SeverityError,
				MatchedLines: []string{line},
				LineCount:    n,
			})
			break // one crash-loop anomaly per scan is enough
		}
	}

	return matches
}

// LogScanner periodically scans recent application logs of every ready
// environment for anomaly patterns and emits runtime.log_anomaly events.
type LogScanner struct {
	db       *models.DB
	awsSvc   *awssvc.Service
	eventSvc *events.Service
	hub      *ws.Hub

	mu      sync.Mutex
	workers map[uuid.UUID]context.CancelFunc
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewLogScanner(db *models.DB, awsSvc *awssvc.Service, eventSvc *events.Service, hub *ws.Hub) *LogScanner {
	ctx, cancel := context.WithCancel(context.Background())
	return &LogScanner{
		db:       db,
		awsSvc:   awsSvc,
		eventSvc: eventSvc,
		hub:      hub,
		workers:  make(map[uuid.UUID]context.CancelFunc),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start blocks, supervising one scanning goroutine per ready environment.
func (l *LogScanner) Start() {
	l.syncWorkers()
	ticker := time.NewTicker(workerSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			l.syncWorkers()
		}
	}
}

// Stop cancels the supervisor and all workers.
func (l *LogScanner) Stop() {
	l.cancel()
}

func (l *LogScanner) syncWorkers() {
	ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
	defer cancel()

	rows, err := l.db.Pool.Query(ctx,
		`SELECT id, project_id, name FROM environments
		 WHERE stack_status = 'ready' AND is_preview = false AND log_group_name IS NOT NULL`)
	if err != nil {
		slog.Error("logscanner: failed to list environments", "component", "monitor.logscanner", "error", err)
		return
	}

	current := make(map[uuid.UUID]monitoredEnv)
	for rows.Next() {
		var e monitoredEnv
		if err := rows.Scan(&e.id, &e.projectID, &e.name); err == nil {
			current[e.id] = e
		}
	}
	rows.Close()

	// Discovered ECS services assigned to a project that expose a log group in metadata.
	drows, err := l.db.Pool.Query(ctx,
		`SELECT id, project_id, resource_name, aws_account_id, region, metadata->>'log_group_name'
		 FROM discovered_resources
		 WHERE resource_type = 'ecs_service' AND project_id IS NOT NULL
		   AND metadata->>'log_group_name' IS NOT NULL AND metadata->>'log_group_name' <> ''`)
	if err == nil {
		for drows.Next() {
			var e monitoredEnv
			var acct uuid.UUID
			if err := drows.Scan(&e.id, &e.projectID, &e.name, &acct, &e.region, &e.logGroup); err != nil {
				continue
			}
			e.discovered = true
			e.accountID = &acct
			current[e.id] = e
		}
		drows.Close()
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for id, stop := range l.workers {
		if _, ok := current[id]; !ok {
			stop()
			delete(l.workers, id)
		}
	}
	for id, env := range current {
		if _, ok := l.workers[id]; ok {
			continue
		}
		wctx, wcancel := context.WithCancel(l.ctx)
		l.workers[id] = wcancel
		go l.runWorker(wctx, env)
	}
}

func (l *LogScanner) runWorker(ctx context.Context, env monitoredEnv) {
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.safeScan(ctx, env)
		}
	}
}

func (l *LogScanner) safeScan(ctx context.Context, env monitoredEnv) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("logscanner: panic recovered", "component", "monitor.logscanner",
				"environment_id", env.id, "panic", r)
		}
	}()
	if err := l.scanOnce(ctx, env); err != nil {
		slog.Warn("logscanner: scan failed", "component", "monitor.logscanner",
			"project_id", env.projectID, "environment_id", env.id, "error", err)
	}
}

func (l *LogScanner) scanOnce(ctx context.Context, env monitoredEnv) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var logGroup string
	var clients *awssvc.ClientBundle

	if env.discovered {
		if env.accountID == nil || env.logGroup == "" {
			return nil
		}
		c, err := l.awsSvc.AssumeRoleForAccountAndRegion(ctx, *env.accountID, env.region)
		if err != nil {
			return err
		}
		clients, logGroup = c, env.logGroup
	} else {
		var fullEnv models.Environment
		err := l.db.Pool.QueryRow(ctx,
			`SELECT id, project_id, account_id, aws_region, log_group_name
			 FROM environments WHERE id = $1`, env.id,
		).Scan(&fullEnv.ID, &fullEnv.ProjectID, &fullEnv.AccountID, &fullEnv.AWSRegion, &fullEnv.LogGroupName)
		if err != nil {
			return err
		}
		if fullEnv.LogGroupName == nil || fullEnv.AccountID == nil {
			return nil
		}
		c, err := l.awsSvc.AssumeRoleForEnvironment(ctx, &fullEnv)
		if err != nil {
			return err
		}
		clients, logGroup = c, *fullEnv.LogGroupName
	}

	lines, err := l.awsSvc.FetchRecentECSLogs(ctx, clients, logGroup, scanLogLines)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return nil
	}

	for _, match := range ScanLines(lines) {
		reported, err := l.recentlyReported(ctx, env.id, match.PatternType)
		if err != nil || reported {
			continue
		}
		envID := env.id
		l.eventSvc.Emit(ctx, events.Event{
			ProjectID:     env.projectID,
			EnvironmentID: &envID,
			Type:          models.EventRuntimeLogAnomaly,
			Severity:      match.Severity,
			Source:        models.SourceScheduler,
			Payload: map[string]any{
				"env_name":      env.name,
				"pattern_type":  match.PatternType,
				"matched_lines": match.MatchedLines,
				"line_count":    match.LineCount,
			},
		})
		l.hub.Broadcast(env.projectID.String(), ws.Message{
			Type:    "runtime_event",
			Payload: "log anomaly (" + match.PatternType + ") detected in " + env.name,
		})
	}
	return nil
}

// recentlyReported checks whether the same anomaly pattern was already reported
// for this environment within the dedup window.
func (l *LogScanner) recentlyReported(ctx context.Context, envID uuid.UUID, patternType string) (bool, error) {
	var exists bool
	err := l.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM operational_events
			WHERE environment_id = $1
			  AND event_type = $2
			  AND payload->>'pattern_type' = $3
			  AND occurred_at > NOW() - $4::interval
		)`, envID, models.EventRuntimeLogAnomaly, patternType, anomalyDedupSpan.String(),
	).Scan(&exists)
	return exists, err
}
