package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ashborntechnologies-web/OpsPilot/internal/billing"
	"github.com/ashborntechnologies-web/OpsPilot/internal/deploy"
	"github.com/ashborntechnologies-web/OpsPilot/internal/diagnosis"
	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/internal/prompts"
	"github.com/ashborntechnologies-web/OpsPilot/internal/trust"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Service struct {
	db           *models.DB
	deploySvc    *deploy.Service
	diagnosisSvc *diagnosis.Service
	llm          *llm.Client
	hub          *ws.Hub
	billing      *billing.Service
	trust        *trust.Service
}

// SetBillingService enables AI-action metering (set once at startup).
func (s *Service) SetBillingService(b *billing.Service) { s.billing = b }

// SetTrustService routes AI-mediated actions (deploy/rollback/scale/change_resources)
// through the trust/approval layer instead of executing directly.
func (s *Service) SetTrustService(t *trust.Service) { s.trust = t }

type IntentResult struct {
	Intent     string                 `json:"intent"`
	Confidence string                 `json:"confidence"`
	Params     map[string]interface{} `json:"params"`
}

func NewService(db *models.DB, deploySvc *deploy.Service, diagnosisSvc *diagnosis.Service, apiKey string, hub *ws.Hub) *Service {
	return &Service{
		db:           db,
		deploySvc:    deploySvc,
		diagnosisSvc: diagnosisSvc,
		llm:          llm.New(apiKey),
		hub:          hub,
	}
}

// HandleMessage is the REST fallback for conversation (primary channel is WebSocket).
func (s *Service) HandleMessage(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	var req struct {
		Message string `json:"message" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	response, err := s.ProcessMessage(c.Request.Context(), projectID, userID, req.Message)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"response": response})
}

// HandleHistory returns the conversation history for a project.
func (s *Service) HandleHistory(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	// Pagination: ?limit= (default 100, max 500) and ?offset= count back from the
	// most recent turn; results are returned oldest-first for rendering.
	limit := 100
	if l, err := strconv.Atoi(c.Query("limit")); err == nil && l > 0 {
		limit = min(l, 500)
	}
	offset := 0
	if o, err := strconv.Atoi(c.Query("offset")); err == nil && o > 0 {
		offset = o
	}

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT id, project_id, user_id, role, message, intent, created_at FROM (
			SELECT id, project_id, user_id, role, message, intent, created_at
			FROM conversations WHERE project_id = $1
			ORDER BY created_at DESC LIMIT $2 OFFSET $3
		 ) page ORDER BY created_at ASC`,
		projectID, limit, offset,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch history"})
		return
	}
	defer rows.Close()

	var messages []models.Conversation
	for rows.Next() {
		var m models.Conversation
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.UserID, &m.Role, &m.Message, &m.Intent, &m.CreatedAt); err != nil {
			continue
		}
		messages = append(messages, m)
	}

	if messages == nil {
		messages = []models.Conversation{}
	}
	c.JSON(http.StatusOK, messages)
}

// ProcessMessage classifies intent, executes the matching workflow, and persists both
// the user message and the assistant response to the conversations table.
// Implements ws.MessageHandler.
func (s *Service) ProcessMessage(ctx context.Context, projectID uuid.UUID, userID uuid.UUID, message string) (string, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return "Please type a message — for example \"deploy\" or \"show me the logs\".", nil
	}
	if len(message) > 4000 {
		return "That message is too long for me to process — please keep it under 4000 characters.", nil
	}

	// AI-action metering — classification calls Claude, so it counts against the
	// workspace's plan allowance (metered per org — ADR-017). Checked before any work so
	// the limit message is immediate.
	if s.billing != nil {
		var orgID uuid.UUID
		if err := s.db.Pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, projectID).Scan(&orgID); err == nil && orgID != uuid.Nil {
			if err := s.billing.IncrementAIAction(ctx, orgID); err != nil {
				var limitErr *billing.ErrLimitReached
				if errors.As(err, &limitErr) {
					return limitErr.Message, nil
				}
				// Metering infrastructure failure — don't block the user.
				slog.Error(fmt.Sprintf("[conversation] AI metering failed: %v", err))
			}
		}
	}

	// Recent conversation context lets the classifier resolve references like
	// "that", "it", or "the same environment".
	contextualMessage := message
	if convContext := s.recentContext(ctx, projectID, 10); convContext != "" {
		contextualMessage = fmt.Sprintf(
			"Recent conversation context:\n%s\n\nCurrent message: %s",
			convContext, message)
	}

	// Persist user message first so history is correct even if processing fails
	s.saveMessage(ctx, projectID, userID, "user", message, nil)

	// Classify intent. On failure (API outage, rate limit) degrade to a helpful
	// fallback response instead of breaking the chat with a raw error.
	intent, err := s.classifyIntent(ctx, contextualMessage)
	if err != nil {
		slog.Error(fmt.Sprintf("[conversation] intent classification failed: %v", err))
		fallback := "I'm having trouble understanding requests right now (the AI service is unavailable). " +
			"You can still use the dashboard buttons to deploy, roll back, or view logs — or try again in a moment."
		s.saveMessage(ctx, projectID, userID, "assistant", fallback, nil)
		return fallback, nil
	}

	// RBAC: viewers may read/ask but cannot trigger actions over chat. The WS layer
	// only verifies membership, so action enforcement happens here.
	if isActionIntent(intent.Intent) {
		if _, role, rerr := s.db.ProjectOrgRole(ctx, userID, projectID); rerr == nil &&
			models.RoleRank(role) < models.RoleRank(models.RoleEngineer) {
			msg := "You have view-only (viewer) access to this workspace, so I can't run that action. " +
				"I can still show you logs, health, costs, or diagnose issues. Ask an admin to grant you the engineer role to deploy, roll back, or scale."
			s.saveMessage(ctx, projectID, userID, "assistant", msg, &intent.Intent)
			return msg, nil
		}
	}

	// Route to the matching workflow — Go code executes, Claude never touches AWS.
	// Action intents (deploy/rollback/scale/change_resources) are AI-mediated, so they go
	// through the trust/approval layer when wired; read intents run directly.
	var response string
	switch intent.Intent {
	case models.IntentDeploy, models.IntentRedeploy:
		response, err = s.act(ctx, projectID, userID, models.ActionDeploy, targetEnv(intent.Params), nil,
			"User asked to deploy via chat.", func() (string, error) {
				return s.deploySvc.TriggerDeploy(ctx, projectID, targetEnv(intent.Params))
			})
	case models.IntentRollback:
		response, err = s.act(ctx, projectID, userID, models.ActionRollback, targetEnv(intent.Params), nil,
			"User asked to roll back via chat.", func() (string, error) {
				return s.deploySvc.TriggerRollback(ctx, projectID, targetEnv(intent.Params))
			})
	case models.IntentLogs:
		response, err = s.deploySvc.FetchLogsForProject(ctx, projectID)
	case models.IntentHealth:
		response, err = s.deploySvc.CheckHealth(ctx, projectID)
	case models.IntentScale:
		// Never guess a replica count — "scale down" silently becoming 2 replicas
		// (or 2 becoming a scale-UP) is a production safety problem.
		replicas, ok := paramInt(intent.Params, "replicas")
		if !ok {
			response = "How many replicas would you like? For example: \"scale to 3\" or \"scale to 0\" to stop the service."
		} else {
			response, err = s.act(ctx, projectID, userID, models.ActionScale, targetEnv(intent.Params),
				map[string]any{"replicas": replicas}, fmt.Sprintf("User asked to scale to %d replicas via chat.", replicas),
				func() (string, error) { return s.deploySvc.ScaleService(ctx, projectID, replicas) })
		}
	case models.IntentDiagnose:
		response, err = s.diagnosisSvc.DiagnoseProject(ctx, projectID)
	case models.IntentCost:
		response, err = s.deploySvc.GetCostSummary(ctx, projectID)
	case models.IntentChangeResources:
		cpu := paramString(intent.Params, "cpu")
		memory := paramString(intent.Params, "memory")
		response, err = s.act(ctx, projectID, userID, models.ActionChangeResources, targetEnv(intent.Params),
			map[string]any{"cpu": cpu, "memory": memory}, "User asked to change compute resources via chat.",
			func() (string, error) { return s.deploySvc.ProposeResourceChange(ctx, projectID, userID, cpu, memory) })
	case models.IntentConfirm:
		response, err = s.deploySvc.ApplyPendingMutation(ctx, projectID, userID)
	default:
		response = "I can help you deploy, rollback, scale, check logs, diagnose issues, view costs, or change compute resources. What would you like to do?"
	}

	if err != nil {
		response = fmt.Sprintf("Something went wrong: %s", err.Error())
	}

	// Persist assistant response
	s.saveMessage(ctx, projectID, userID, "assistant", response, &intent.Intent)

	return response, nil
}

// act routes an AI-mediated action through the trust/approval layer. When trust is wired,
// it proposes the action (which auto-executes only if the environment's trust level +
// boundaries allow it) and returns a status-appropriate chat reply. When trust is not
// wired, it falls back to executing directly (direct) for backward compatibility.
func (s *Service) act(ctx context.Context, projectID, userID uuid.UUID, actionType, envName string, params map[string]any, rationale string, direct func() (string, error)) (string, error) {
	if s.trust == nil {
		return direct()
	}
	orgID, _, _ := s.db.ProjectOrgRole(ctx, userID, projectID)
	uid := userID
	a, err := s.trust.ProposeAction(ctx, trust.ActionProposal{
		OrgID:            orgID,
		ProjectID:        projectID,
		EnvName:          envName,
		ProposedByType:   models.ProposerAI,
		ProposedByUserID: &uid,
		ActionType:       actionType,
		Parameters:       params,
		Rationale:        rationale,
	})
	if err != nil {
		return "", err
	}
	switch a.Status {
	case models.ActionStatusExecuted:
		msg, _ := a.Result["message"].(string)
		if msg == "" {
			msg = fmt.Sprintf("Done — %s executed on %s.", actionType, envName)
		}
		return msg, nil
	case models.ActionStatusFailed:
		errMsg, _ := a.Result["error"].(string)
		return fmt.Sprintf("The %s action failed: %s", actionType, errMsg), nil
	default:
		return fmt.Sprintf("I've proposed a %s action on %s — it needs approval before it runs. "+
			"An engineer or admin can approve it from the Pending Approvals panel.", actionType, envName), nil
	}
}

// recentContext renders the last N conversation turns oldest-first as a compact
// context block for the intent classifier.
func (s *Service) recentContext(ctx context.Context, projectID uuid.UUID, limit int) string {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT role, message FROM (
			SELECT role, message, created_at FROM conversations
			WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2
		) t ORDER BY created_at ASC`, projectID, limit)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var role, msg string
		if rows.Scan(&role, &msg) != nil {
			continue
		}
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		fmt.Fprintf(&b, "[%s]: %s\n", role, msg)
	}
	return strings.TrimSpace(b.String())
}

// isActionIntent reports whether an intent triggers an infrastructure action
// (forbidden for viewers). Read-only intents (logs/health/diagnose/cost) are allowed.
func isActionIntent(intent string) bool {
	switch intent {
	case models.IntentDeploy, models.IntentRedeploy, models.IntentRollback,
		models.IntentScale, models.IntentChangeResources, models.IntentConfirm:
		return true
	}
	return false
}

// targetEnv returns the environment named in the intent params, defaulting to
// production. Only known environment names are honored.
func targetEnv(params map[string]interface{}) string {
	env := strings.ToLower(paramString(params, "env"))
	if env == "staging" || env == "production" {
		return env
	}
	return "production"
}

// paramString reads a classifier param that should be a string but may arrive
// as a JSON number (Claude occasionally returns 1024 instead of "1024").
func paramString(params map[string]interface{}, key string) string {
	switch v := params[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// paramInt reads an integer classifier param that may arrive as a number or string.
func paramInt(params map[string]interface{}, key string) (int, bool) {
	switch v := params[key].(type) {
	case float64:
		return int(v), true
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// classifyIntent sends the message to Claude and returns the parsed intent.
func (s *Service) classifyIntent(ctx context.Context, message string) (*IntentResult, error) {
	text, err := s.llm.Complete(ctx, prompts.IntentClassifier(), message, 200)
	if err != nil {
		return nil, err
	}

	var result IntentResult
	if err := json.Unmarshal([]byte(stripJSONFences(text)), &result); err != nil {
		return nil, fmt.Errorf("failed to parse intent JSON: %w", err)
	}
	return &result, nil
}

// stripJSONFences removes markdown code fences the model occasionally wraps
// around JSON despite instructions.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// saveMessage persists a conversation turn. Errors are logged but never surfaced —
// a failed DB write should not break the user's conversation.
func (s *Service) saveMessage(ctx context.Context, projectID, userID uuid.UUID, role, message string, intent *string) {
	s.db.Pool.Exec(ctx,
		`INSERT INTO conversations (project_id, user_id, role, message, intent)
		 VALUES ($1, $2, $3, $4, $5)`,
		projectID, userID, role, message, intent,
	)
}
