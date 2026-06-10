// Package export produces the platform's proprietary training datasets as JSONL
// in OpenAI fine-tuning format. These exports are trade secrets: the intent
// dataset captures real operator phrasing → intent labels, and the diagnosis
// dataset contains only user-verified fixes (rating=helpful AND fixed_issue).
package export

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
)

type Service struct {
	db *models.DB
}

func NewService(db *models.DB) *Service {
	return &Service{db: db}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type trainingRow struct {
	Messages []message `json:"messages"`
}

func writeRow(w io.Writer, userContent, assistantContent string) error {
	row := trainingRow{Messages: []message{
		{Role: "user", Content: userContent},
		{Role: "assistant", Content: assistantContent},
	}}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ExportIntentTrainingData streams (user message → classified intent) pairs as
// JSONL. Only successful interactions are included: assistant turns whose action
// errored ("Something went wrong...") or fell back ("I'm having trouble...") are
// excluded, as are unknown intents.
func (s *Service) ExportIntentTrainingData(ctx context.Context, since time.Time, w io.Writer) (int, error) {
	// Pair each classified assistant turn with the immediately preceding user
	// message in the same project via a window function.
	rows, err := s.db.Pool.Query(ctx, `
		SELECT user_msg, intent FROM (
			SELECT role, message, intent, created_at,
			       LAG(message) OVER (PARTITION BY project_id ORDER BY created_at) AS user_msg,
			       LAG(role)    OVER (PARTITION BY project_id ORDER BY created_at) AS prev_role
			FROM conversations
		) t
		WHERE role = 'assistant'
		  AND intent IS NOT NULL
		  AND intent <> 'unknown'
		  AND prev_role = 'user'
		  AND user_msg IS NOT NULL
		  AND message NOT LIKE 'Something went wrong%'
		  AND message NOT LIKE 'I''m having trouble%'
		  AND created_at >= $1
		ORDER BY created_at ASC`, since)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var userMsg, intent string
		if err := rows.Scan(&userMsg, &intent); err != nil {
			continue
		}
		target, _ := json.Marshal(map[string]string{"intent": intent})
		if err := writeRow(w, userMsg, string(target)); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

// ExportDiagnosisTrainingData streams the gold-standard verified-fix dataset:
// incidents whose diagnosis a user rated helpful AND confirmed fixed the issue.
// user = the diagnostic context (logs), assistant = the structured diagnosis.
func (s *Service) ExportDiagnosisTrainingData(ctx context.Context, since time.Time, w io.Writer) (int, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT COALESCE(i.raw_logs, ''), COALESCE(i.root_cause, ''), COALESCE(i.resolution, '')
		FROM incidents i
		JOIN diagnosis_feedback f ON f.incident_id = i.id
		WHERE f.rating = 'helpful' AND f.fixed_issue = true
		  AND i.created_at >= $1
		ORDER BY i.created_at ASC`, since)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var logs, rootCause, resolution string
		if err := rows.Scan(&logs, &rootCause, &resolution); err != nil {
			continue
		}
		if logs == "" || rootCause == "" {
			continue
		}
		assistant := fmt.Sprintf("**Root Cause:** %s", rootCause)
		if resolution != "" {
			assistant += fmt.Sprintf("\n**Fix:** %s", resolution)
		}
		if err := writeRow(w, logs, assistant); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

// parseSince reads ?since=YYYY-MM-DD; absent means the beginning of time.
func parseSince(c *gin.Context) (time.Time, error) {
	raw := c.Query("since")
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("since must be YYYY-MM-DD")
	}
	return t, nil
}

// HandleExportIntents — GET /api/v1/admin/export/intents?since=YYYY-MM-DD
func (s *Service) HandleExportIntents(c *gin.Context) {
	s.handleExport(c, "opspilot-intents", s.ExportIntentTrainingData)
}

// HandleExportDiagnoses — GET /api/v1/admin/export/diagnoses?since=YYYY-MM-DD
func (s *Service) HandleExportDiagnoses(c *gin.Context) {
	s.handleExport(c, "opspilot-diagnoses", s.ExportDiagnosisTrainingData)
}

func (s *Service) handleExport(c *gin.Context, name string, fn func(context.Context, time.Time, io.Writer) (int, error)) {
	since, err := parseSince(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".jsonl"))
	c.Status(http.StatusOK)

	if _, err := fn(c.Request.Context(), since, c.Writer); err != nil {
		// Headers are already sent — log only; the truncated stream signals failure.
		_ = c.Error(err)
	}
}
