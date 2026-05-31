package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ashborntechnologies-web/OpsPilot/internal/deploy"
	"github.com/ashborntechnologies-web/OpsPilot/internal/diagnosis"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const anthropicAPIURL = "https://api.anthropic.com/v1/messages"
const claudeModel = "claude-sonnet-4-20250514"

const intentClassifierPrompt = `You are the intent classifier for a deployment platform.
Classify the user's message into exactly one of these intents:
- deploy: user wants to deploy or redeploy their application
- rollback: user wants to roll back to a previous deployment
- scale: user wants to scale replicas up or down
- logs: user wants to view logs
- health: user wants to check deployment health or status
- diagnose: user wants to understand why something failed or what went wrong
- unknown: anything else

Respond with ONLY a JSON object in this exact format:
{"intent": "<intent>", "confidence": "<high|medium|low>", "params": {}}

For scale intent, extract replica count into params: {"replicas": 3}
For rollback, extract deployment reference if mentioned: {"deployment_id": "..."}
Never include any other text.`

type Service struct {
	db           *models.DB
	deploySvc    *deploy.Service
	diagnosisSvc *diagnosis.Service
	apiKey       string
	hub          *ws.Hub
}

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system"`
	Messages  []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

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
		apiKey:       apiKey,
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

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT id, project_id, user_id, role, message, intent, created_at
		 FROM conversations WHERE project_id = $1 ORDER BY created_at ASC LIMIT 100`,
		projectID,
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
	// Persist user message first so history is correct even if processing fails
	s.saveMessage(ctx, projectID, userID, "user", message, nil)

	// Classify intent
	intent, err := s.classifyIntent(ctx, message)
	if err != nil {
		return "", fmt.Errorf("failed to classify intent: %w", err)
	}

	// Route to the matching workflow — Go code executes, Claude never touches AWS
	var response string
	switch intent.Intent {
	case models.IntentDeploy, models.IntentRedeploy:
		response, err = s.deploySvc.TriggerDeploy(ctx, projectID, "production")
	case models.IntentRollback:
		response, err = s.deploySvc.TriggerRollback(ctx, projectID, "production")
	case models.IntentLogs:
		response, err = s.deploySvc.FetchLogsForProject(ctx, projectID)
	case models.IntentHealth:
		response, err = s.deploySvc.CheckHealth(ctx, projectID)
	case models.IntentScale:
		replicas := 2
		if r, ok := intent.Params["replicas"]; ok {
			if rf, ok := r.(float64); ok {
				replicas = int(rf)
			}
		}
		response, err = s.deploySvc.ScaleService(ctx, projectID, replicas)
	case models.IntentDiagnose:
		response, err = s.diagnosisSvc.DiagnoseProject(ctx, projectID)
	default:
		response = "I can help you deploy, rollback, scale, check logs, or diagnose issues. What would you like to do?"
	}

	if err != nil {
		response = fmt.Sprintf("Something went wrong: %s", err.Error())
	}

	// Persist assistant response
	s.saveMessage(ctx, projectID, userID, "assistant", response, &intent.Intent)

	return response, nil
}

// classifyIntent sends the message to Claude and returns the parsed intent.
func (s *Service) classifyIntent(ctx context.Context, message string) (*IntentResult, error) {
	reqBody := claudeRequest{
		Model:     claudeModel,
		MaxTokens: 200,
		System:    intentClassifierPrompt,
		Messages:  []claudeMessage{{Role: "user", Content: message}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var claudeResp claudeResponse
	if err := json.Unmarshal(respBody, &claudeResp); err != nil {
		return nil, err
	}

	if len(claudeResp.Content) == 0 {
		return nil, fmt.Errorf("empty response from Claude")
	}

	var result IntentResult
	if err := json.Unmarshal([]byte(claudeResp.Content[0].Text), &result); err != nil {
		return nil, fmt.Errorf("failed to parse intent JSON: %w", err)
	}

	return &result, nil
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
