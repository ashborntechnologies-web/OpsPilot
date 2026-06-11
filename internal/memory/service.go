// Package memory is OpsPilot's long-term project memory. It records what the
// platform learns about each project — confirmed fixes, recurring failures,
// deploy timing patterns — and serves the most relevant facts back into
// diagnosis prompts, so the AI gets smarter about a project the longer it runs.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/internal/llm"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

type Service struct {
	db  *models.DB
	llm *llm.Client
}

func NewService(db *models.DB, llmClient *llm.Client) *Service {
	return &Service{db: db, llm: llmClient}
}

// contentKey normalizes memory content for near-duplicate detection: lowercase,
// whitespace-collapsed, truncated. Two memories with the same key are treated
// as the same fact (reference_count++ rather than a new row).
func contentKey(content string) string {
	key := strings.ToLower(strings.Join(strings.Fields(content), " "))
	if len(key) > 120 {
		key = key[:120]
	}
	return key
}

// upsert inserts a memory or, when an existing row of the same type has
// near-identical content, bumps its reference count and confidence instead.
func (s *Service) upsert(ctx context.Context, projectID uuid.UUID, memoryType, content, source string, confidence float64) error {
	key := contentKey(content)

	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, content FROM project_memory
		 WHERE project_id = $1 AND memory_type = $2
		 ORDER BY last_referenced_at DESC LIMIT 20`,
		projectID, memoryType)
	if err != nil {
		return err
	}
	defer rows.Close()

	var existingID *uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var existing string
		if rows.Scan(&id, &existing) != nil {
			continue
		}
		if contentKey(existing) == key {
			existingID = &id
			break
		}
	}
	rows.Close()

	if existingID != nil {
		_, err = s.db.Pool.Exec(ctx, `
			UPDATE project_memory
			SET reference_count = reference_count + 1,
			    confidence = LEAST(1.0, GREATEST(confidence, $1)),
			    content = $2,
			    last_referenced_at = NOW()
			WHERE id = $3`,
			confidence, content, *existingID)
		return err
	}

	_, err = s.db.Pool.Exec(ctx, `
		INSERT INTO project_memory (project_id, memory_type, content, confidence, source)
		VALUES ($1, $2, $3, $4, $5)`,
		projectID, memoryType, content, confidence, source)
	return err
}

// RecordDiagnosisFeedback persists a confirmed fix as a high-confidence memory.
// Call when feedback is rating=helpful AND fixed_issue=true.
func (s *Service) RecordDiagnosisFeedback(ctx context.Context, projectID, incidentID uuid.UUID) {
	var rootCause, resolution string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT COALESCE(root_cause, ''), COALESCE(resolution, '')
		 FROM incidents WHERE id = $1 AND project_id = $2`,
		incidentID, projectID,
	).Scan(&rootCause, &resolution)
	if err != nil || rootCause == "" {
		return
	}

	content := fmt.Sprintf("When seeing %q, the confirmed fix was: %s", truncate(rootCause, 200), truncate(resolution, 300))
	if resolution == "" {
		content = fmt.Sprintf("A diagnosis of %q was confirmed accurate by the user.", truncate(rootCause, 200))
	}
	if err := s.upsert(ctx, projectID, models.MemorySuccessfulFix, content, models.MemorySourceUserConfirmed, 1.0); err != nil {
		slog.Warn("memory: failed to record confirmed fix", "component", "memory", "project_id", projectID, "error", err)
	}

	// Recurring failure detection: same root cause appearing 2+ times.
	var occurrences int
	s.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM incidents WHERE project_id = $1 AND root_cause = (SELECT root_cause FROM incidents WHERE id = $2)`,
		projectID, incidentID,
	).Scan(&occurrences)
	if occurrences >= 2 {
		s.RecordRecurringFailure(ctx, projectID,
			fmt.Sprintf("This project has hit the same failure %d times: %s", occurrences, truncate(rootCause, 200)),
			occurrences)
	}
}

// RecordRecurringFailure stores a repeated-failure pattern with confidence
// proportional to how often it has recurred.
func (s *Service) RecordRecurringFailure(ctx context.Context, projectID uuid.UUID, failurePattern string, occurrences int) {
	confidence := float64(occurrences) / 10.0
	if confidence > 1.0 {
		confidence = 1.0
	}
	if err := s.upsert(ctx, projectID, models.MemoryRecurringFailure, failurePattern, models.MemorySourcePatternDetected, confidence); err != nil {
		slog.Warn("memory: failed to record recurring failure", "component", "memory", "project_id", projectID, "error", err)
	}
}

// RecordDeployPattern recomputes the project's deploy timing/success profile
// from the last 10 deployments. Called after every completed deploy.
func (s *Service) RecordDeployPattern(ctx context.Context, projectID uuid.UUID) {
	var avgMinutes float64
	var successPct float64
	err := s.db.Pool.QueryRow(ctx, `
		SELECT COALESCE(AVG(EXTRACT(EPOCH FROM (updated_at - created_at)) / 60), 0),
		       COALESCE(AVG(CASE WHEN status IN ('live', 'rolled_back') THEN 100.0 ELSE 0.0 END), 0)
		FROM (
			SELECT status, created_at, updated_at FROM deployments
			WHERE project_id = $1 AND status IN ('live', 'failed', 'rolled_back')
			ORDER BY created_at DESC LIMIT 10
		) recent`, projectID,
	).Scan(&avgMinutes, &successPct)
	if err != nil {
		return
	}

	content := fmt.Sprintf("Deploys to this project typically take %.0f minutes and have a %.0f%% success rate (based on last 10).",
		avgMinutes, successPct)

	// Deploy pattern is a single evolving fact — replace in place.
	tag, err := s.db.Pool.Exec(ctx, `
		UPDATE project_memory SET content = $1, last_referenced_at = NOW(), reference_count = reference_count + 1
		WHERE project_id = $2 AND memory_type = $3`,
		content, projectID, models.MemoryDeployPattern)
	if err != nil {
		return
	}
	if tag.RowsAffected() == 0 {
		s.db.Pool.Exec(ctx, `
			INSERT INTO project_memory (project_id, memory_type, content, confidence, source)
			VALUES ($1, $2, $3, 1.0, $4)`,
			projectID, models.MemoryDeployPattern, content, models.MemorySourcePatternDetected)
	}
}

// GetRelevantMemory returns the highest-signal memories for a project, ranked
// by confidence × reference frequency, and touches their last_referenced_at.
func (s *Service) GetRelevantMemory(ctx context.Context, projectID uuid.UUID, limit int) ([]models.ProjectMemory, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, project_id, memory_type, content, confidence, source,
		       created_at, last_referenced_at, reference_count
		FROM project_memory
		WHERE project_id = $1
		ORDER BY (confidence * reference_count) DESC, last_referenced_at DESC
		LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []models.ProjectMemory
	var ids []uuid.UUID
	for rows.Next() {
		var m models.ProjectMemory
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.MemoryType, &m.Content, &m.Confidence,
			&m.Source, &m.CreatedAt, &m.LastReferencedAt, &m.ReferenceCount); err != nil {
			continue
		}
		memories = append(memories, m)
		ids = append(ids, m.ID)
	}
	rows.Close()

	if len(ids) > 0 {
		ctxTouch, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.db.Pool.Exec(ctxTouch, `UPDATE project_memory SET last_referenced_at = NOW() WHERE id = ANY($1)`, ids)
	}
	return memories, nil
}

// FormatForPrompt renders memories as a prompt section. Empty when no memory exists.
func FormatForPrompt(memories []models.ProjectMemory) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Project Memory (facts learned from this project's history)\n")
	for _, m := range memories {
		fmt.Fprintf(&b, "- [%s] %s\n", m.MemoryType, m.Content)
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
