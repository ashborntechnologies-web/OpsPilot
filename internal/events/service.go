package events

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

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

type Service struct {
	db *models.DB
}

func NewService(db *models.DB) *Service {
	return &Service{db: db}
}

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
		_, err := s.db.Pool.Exec(context.Background(),
			`INSERT INTO operational_events
			 (project_id, environment_id, deployment_id, event_type, severity, source, actor_type, payload)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			ev.ProjectID, ev.EnvironmentID, ev.DeploymentID,
			ev.Type, ev.Severity, ev.Source, ev.ActorType, payloadJSON,
		)
		if err != nil {
			log.Printf("events.Emit: failed to persist %s: %v", ev.Type, err)
		}
	}()
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
			log.Printf("events.EmitAccount: failed to persist %s: %v", eventType, err)
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
