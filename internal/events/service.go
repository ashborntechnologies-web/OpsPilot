package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Event is the input to Emit. All fields except ProjectID, Type are optional.
type Event struct {
	ProjectID     uuid.UUID
	EnvironmentID *uuid.UUID
	DeploymentID  *uuid.UUID
	Type          string
	Severity      string         // defaults to models.SeverityInfo
	Source        string         // defaults to models.SourceDeployer
	ActorType     string         // defaults to models.ActorSystem
	Payload       map[string]any // optional structured context
}

// AlertEvaluator is implemented by monitor.AlertEngine. Defined here as an
// interface to avoid an import cycle (monitor imports events).
type AlertEvaluator interface {
	EvaluateEvent(ctx context.Context, ev models.OperationalEvent)
}

// DiagnosisEnqueuer is implemented by queue.Client. An empty deploymentID means
// "diagnose the current runtime state" rather than a specific deployment.
type DiagnosisEnqueuer interface {
	EnqueueDiagnose(projectID, deploymentID string) error
}

type Service struct {
	db *models.DB

	alertEngine       AlertEvaluator
	diagnosisEnqueuer DiagnosisEnqueuer
}

func NewService(db *models.DB) *Service {
	return &Service{db: db}
}

// SetAlertEngine hooks the alert engine into event emission. Set once at startup.
func (s *Service) SetAlertEngine(a AlertEvaluator) { s.alertEngine = a }

// SetDiagnosisEnqueuer enables autonomous diagnosis of runtime anomalies.
func (s *Service) SetDiagnosisEnqueuer(d DiagnosisEnqueuer) { s.diagnosisEnqueuer = d }

// Emit persists an operational event asynchronously. Errors are logged, never returned.
func (s *Service) Emit(ctx context.Context, ev Event) {
	if ev.Severity == "" {
		ev.Severity = models.SeverityInfo
	}
	if ev.Source == "" {
		ev.Source = models.SourceDeployer
	}
	if ev.ActorType == "" {
		ev.ActorType = models.ActorSystem
	}

	payloadJSON := json.RawMessage("{}")
	if ev.Payload != nil {
		if b, err := json.Marshal(ev.Payload); err == nil {
			payloadJSON = b
		}
	}

	go func() {
		ctx := context.Background()
		var id uuid.UUID
		var occurredAt time.Time
		err := s.db.Pool.QueryRow(ctx,
			`INSERT INTO operational_events
			 (project_id, environment_id, deployment_id, event_type, severity, source, actor_type, payload)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 RETURNING id, occurred_at`,
			ev.ProjectID, ev.EnvironmentID, ev.DeploymentID,
			ev.Type, ev.Severity, ev.Source, ev.ActorType, payloadJSON,
		).Scan(&id, &occurredAt)
		if err != nil {
			slog.Error(fmt.Sprintf("events.Emit: failed to persist %s: %v", ev.Type, err))
			return
		}

		opEvent := models.OperationalEvent{
			ID:            id,
			ProjectID:     ev.ProjectID,
			EnvironmentID: ev.EnvironmentID,
			DeploymentID:  ev.DeploymentID,
			EventType:     ev.Type,
			Severity:      ev.Severity,
			Source:        ev.Source,
			ActorType:     ev.ActorType,
			Payload:       ev.Payload,
			OccurredAt:    occurredAt,
		}

		// Alert engine sees every event (dedup/snooze happen inside).
		if s.alertEngine != nil {
			s.alertEngine.EvaluateEvent(ctx, opEvent)
		}

		// Autonomous diagnosis: error-severity runtime anomalies trigger an AI
		// root-cause analysis — but never while a deploy is already in flight
		// (the deploy pipeline has its own failure handling).
		if s.diagnosisEnqueuer != nil &&
			ev.Severity == models.SeverityError &&
			strings.HasPrefix(ev.Type, "runtime.") &&
			ev.EnvironmentID != nil &&
			!s.deploymentInFlight(ctx, *ev.EnvironmentID) {
			if err := s.diagnosisEnqueuer.EnqueueDiagnose(ev.ProjectID.String(), ""); err != nil {
				slog.Error(fmt.Sprintf("events.Emit: failed to enqueue autonomous diagnosis: %v", err))
			}
		}
	}()
}

// deploymentInFlight reports whether a deployment is currently running for the
// environment.
func (s *Service) deploymentInFlight(ctx context.Context, envID uuid.UUID) bool {
	var inFlight bool
	err := s.db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM deployments
		  WHERE environment_id = $1 AND status IN ('pending', 'building', 'deploying'))`,
		envID,
	).Scan(&inFlight)
	return err == nil && inFlight
}

// EmitAccount persists an account-scoped operational event (no project context) — e.g.
// external_id.generated at AWS account connection. Uses the same table/service as Emit;
// project_id is left NULL. Errors are logged, never returned.
func (s *Service) EmitAccount(ctx context.Context, accountID uuid.UUID, eventType, severity, source string, payload map[string]any) {
	if severity == "" {
		severity = models.SeverityInfo
	}
	if source == "" {
		source = models.SourceDeployer
	}

	payloadJSON := json.RawMessage("{}")
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil {
			payloadJSON = b
		}
	}

	go func() {
		_, err := s.db.Pool.Exec(context.Background(),
			`INSERT INTO operational_events
			 (account_id, event_type, severity, source, actor_type, payload)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			accountID, eventType, severity, source, models.ActorUser, payloadJSON,
		)
		if err != nil {
			slog.Error(fmt.Sprintf("events.EmitAccount: failed to persist %s: %v", eventType, err))
		}
	}()
}

// HandleGetDeploymentEvents returns all operational events for a given deployment.
func (s *Service) HandleGetDeploymentEvents(c *gin.Context) {
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

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT id, project_id, environment_id, deployment_id, event_type, severity, source, actor_type, payload, occurred_at
		 FROM operational_events
		 WHERE project_id = $1 AND deployment_id = $2
		 ORDER BY occurred_at ASC`,
		projectID, deploymentID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch events"})
		return
	}
	defer rows.Close()

	result := []models.OperationalEvent{}
	for rows.Next() {
		var ev models.OperationalEvent
		var payloadJSON []byte
		if err := rows.Scan(
			&ev.ID, &ev.ProjectID, &ev.EnvironmentID, &ev.DeploymentID,
			&ev.EventType, &ev.Severity, &ev.Source, &ev.ActorType,
			&payloadJSON, &ev.OccurredAt,
		); err != nil {
			continue
		}
		if len(payloadJSON) > 0 {
			var m map[string]any
			if err := json.Unmarshal(payloadJSON, &m); err == nil {
				ev.Payload = m
			}
		}
		result = append(result, ev)
	}

	c.JSON(http.StatusOK, result)
}

// HandleGetProjectEvents returns the most recent operational events across all
// environments of a project — feeds the "Recent Activity" sidebar.
// GET /projects/:id/events?limit=5
func (s *Service) HandleGetProjectEvents(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	limit := 5
	if n, err := strconv.Atoi(c.Query("limit")); err == nil && n > 0 && n <= 50 {
		limit = n
	}

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT id, project_id, environment_id, deployment_id, event_type, severity, source, actor_type, payload, occurred_at
		 FROM operational_events
		 WHERE project_id = $1
		 ORDER BY occurred_at DESC
		 LIMIT $2`,
		projectID, limit,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch events"})
		return
	}
	defer rows.Close()

	result := []models.OperationalEvent{}
	for rows.Next() {
		var ev models.OperationalEvent
		var payloadJSON []byte
		if err := rows.Scan(
			&ev.ID, &ev.ProjectID, &ev.EnvironmentID, &ev.DeploymentID,
			&ev.EventType, &ev.Severity, &ev.Source, &ev.ActorType,
			&payloadJSON, &ev.OccurredAt,
		); err != nil {
			continue
		}
		if len(payloadJSON) > 0 {
			var m map[string]any
			if err := json.Unmarshal(payloadJSON, &m); err == nil {
				ev.Payload = m
			}
		}
		result = append(result, ev)
	}
	c.JSON(http.StatusOK, result)
}
