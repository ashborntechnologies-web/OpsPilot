package postmortem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const pmSelect = `
	SELECT pm.id, pm.incident_id, pm.org_id, pm.project_id, pm.title, pm.status,
	       pm.content_markdown, pm.action_items, pm.generated_at, pm.published_at,
	       pm.published_by, pm.created_at, pm.updated_at
	FROM postmortems pm`

// HandleGetByIncident returns the postmortem for an incident, or 404 {generating:true}
// while it is still being generated. GET /incidents/:incidentId/postmortem — member.
func (s *Service) HandleGetByIncident(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}
	incidentID, err := uuid.Parse(c.Param("incidentId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid incident id"})
		return
	}
	ctx := c.Request.Context()

	// Access check via the incident's org.
	var orgID *uuid.UUID
	var resolved *uuid.UUID // placeholder to detect existence
	if err := s.db.Pool.QueryRow(ctx, `SELECT org_id, resolved_by FROM incidents WHERE id = $1`, incidentID).Scan(&orgID, &resolved); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}
	if orgID == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}
	if _, err := s.db.UserOrgRole(ctx, userID, *orgID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}

	pm, err := s.loadOne(ctx, pmSelect+` WHERE pm.incident_id = $1`, incidentID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Resolved incidents have a generation job in flight; otherwise simply none yet.
		c.JSON(http.StatusNotFound, gin.H{"generating": resolved != nil})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load postmortem"})
		return
	}
	c.JSON(http.StatusOK, pm)
}

// HandleGet returns a single postmortem by ID (for the editor). GET /postmortems/:postmortemId — member.
func (s *Service) HandleGet(c *gin.Context) {
	pmID, _, ok := s.requireRole(c, models.RoleViewer)
	if !ok {
		return
	}
	pm, err := s.loadOne(c.Request.Context(), pmSelect+` WHERE pm.id = $1`, pmID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "postmortem not found"})
		return
	}
	c.JSON(http.StatusOK, pm)
}

// HandleUpdate saves edits to a draft postmortem. PATCH /postmortems/:postmortemId — engineer+.
func (s *Service) HandleUpdate(c *gin.Context) {
	pmID, orgID, ok := s.requireRole(c, models.RoleEngineer)
	if !ok {
		return
	}
	var req struct {
		ContentMarkdown *string              `json:"content_markdown"`
		Title           *string              `json:"title"`
		ActionItems     *[]models.ActionItem `json:"action_items"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var itemsJSON []byte
	if req.ActionItems != nil {
		itemsJSON, _ = json.Marshal(*req.ActionItems)
	}
	_, err := s.db.Pool.Exec(c.Request.Context(), `
		UPDATE postmortems SET
		    content_markdown = COALESCE($2, content_markdown),
		    title = COALESCE($3, title),
		    action_items = COALESCE($4, action_items),
		    updated_at = NOW()
		WHERE id = $1`,
		pmID, req.ContentMarkdown, req.Title, itemsJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save postmortem"})
		return
	}
	_ = orgID
	c.JSON(http.StatusOK, gin.H{"message": "postmortem saved"})
}

// HandlePublish marks a postmortem published. POST /postmortems/:postmortemId/publish — engineer+.
func (s *Service) HandlePublish(c *gin.Context) {
	pmID, _, ok := s.requireRole(c, models.RoleEngineer)
	if !ok {
		return
	}
	userID, _ := middleware.GetUserID(c)
	_, err := s.db.Pool.Exec(c.Request.Context(),
		`UPDATE postmortems SET status = 'published', published_at = NOW(), published_by = $2, updated_at = NOW() WHERE id = $1`,
		pmID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to publish postmortem"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "postmortem published", "status": models.PostmortemPublished})
}

// HandleExport returns the postmortem as a markdown download or print-ready HTML.
// GET /postmortems/:postmortemId/export?format=md|pdf — member.
func (s *Service) HandleExport(c *gin.Context) {
	pmID, _, ok := s.requireRole(c, models.RoleViewer)
	if !ok {
		return
	}
	pm, err := s.loadOne(c.Request.Context(), pmSelect+` WHERE pm.id = $1`, pmID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "postmortem not found"})
		return
	}
	name := slug(pm.Title)
	if c.Query("format") == "pdf" {
		// Print-ready HTML — the browser's "Save as PDF" handles conversion (no native
		// PDF dependency). See ADR-014.
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, printableHTML(pm))
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.md"`, name))
	c.Header("Content-Type", "text/markdown; charset=utf-8")
	c.String(http.StatusOK, pm.ContentMarkdown)
}

// HandleListOrg lists published postmortems for an org (the compliance/SOC2 library), with
// optional filters. GET /orgs/:orgId/postmortems?project_id=&severity=&from=&to=&q= — member.
func (s *Service) HandleListOrg(c *gin.Context) {
	orgID, ok := middleware.GetOrgID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "organization not resolved"})
		return
	}
	where := []string{"pm.org_id = $1", "pm.status = 'published'"}
	args := []any{orgID}
	add := func(clause string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if v := c.Query("project_id"); v != "" {
		if pid, err := uuid.Parse(v); err == nil {
			add("pm.project_id = $%d", pid)
		}
	}
	if v := c.Query("severity"); v != "" {
		add("i.severity = $%d", v)
	}
	if v := c.Query("from"); v != "" {
		add("pm.published_at >= $%d", v)
	}
	if v := c.Query("to"); v != "" {
		add("pm.published_at <= $%d", v)
	}
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		add("pm.content_markdown ILIKE $%d", "%"+v+"%")
	}

	query := `
		SELECT pm.id, pm.incident_id, pm.org_id, pm.project_id, pm.title, pm.status,
		       pm.published_at, pm.created_at,
		       COALESCE(p.name,''), COALESCE(i.title,''), COALESCE(i.severity,''),
		       i.created_at, i.resolved_at
		FROM postmortems pm
		LEFT JOIN projects p  ON p.id = pm.project_id
		LEFT JOIN incidents i ON i.id = pm.incident_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY pm.published_at DESC NULLS LAST LIMIT 200`

	rows, err := s.db.Pool.Query(c.Request.Context(), query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list postmortems"})
		return
	}
	defer rows.Close()
	out := []models.Postmortem{}
	for rows.Next() {
		var pm models.Postmortem
		if rows.Scan(&pm.ID, &pm.IncidentID, &pm.OrgID, &pm.ProjectID, &pm.Title, &pm.Status,
			&pm.PublishedAt, &pm.CreatedAt,
			&pm.ProjectName, &pm.IncidentTitle, &pm.Severity, &pm.IncidentOpened, &pm.IncidentClosed) != nil {
			continue
		}
		out = append(out, pm)
	}
	c.JSON(http.StatusOK, out)
}

// requireRole resolves the postmortem in the path, checks the caller's role in its org,
// and returns the postmortem + org IDs. On failure it writes the response and ok=false.
func (s *Service) requireRole(c *gin.Context, minRole string) (pmID, orgID uuid.UUID, ok bool) {
	userID, authed := middleware.GetUserID(c)
	if !authed {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	pmID, err := uuid.Parse(c.Param("postmortemId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid postmortem id"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	ctx := c.Request.Context()
	if err := s.db.Pool.QueryRow(ctx, `SELECT org_id FROM postmortems WHERE id = $1`, pmID).Scan(&orgID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "postmortem not found"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	role, err := s.db.UserOrgRole(ctx, userID, orgID)
	if errors.Is(err, models.ErrNoMembership) {
		c.JSON(http.StatusNotFound, gin.H{"error": "postmortem not found"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify access"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	if models.RoleRank(role) < models.RoleRank(minRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "this action requires a higher role in the workspace"})
		return uuid.UUID{}, uuid.UUID{}, false
	}
	return pmID, orgID, true
}

// loadOne scans a single postmortem row (pmSelect column order).
func (s *Service) loadOne(ctx context.Context, query string, args ...any) (*models.Postmortem, error) {
	var pm models.Postmortem
	var itemsRaw []byte
	err := s.db.Pool.QueryRow(ctx, query, args...).Scan(
		&pm.ID, &pm.IncidentID, &pm.OrgID, &pm.ProjectID, &pm.Title, &pm.Status,
		&pm.ContentMarkdown, &itemsRaw, &pm.GeneratedAt, &pm.PublishedAt,
		&pm.PublishedBy, &pm.CreatedAt, &pm.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(itemsRaw, &pm.ActionItems)
	return &pm, nil
}

// printableHTML wraps the postmortem markdown in minimal print-styled HTML so a browser
// can "Save as PDF" without a native PDF renderer.
func printableHTML(pm *models.Postmortem) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>%s</title>
<style>
  @media print { @page { margin: 1in; } }
  body { font-family: -apple-system, Segoe UI, Helvetica, Arial, sans-serif; max-width: 720px; margin: 2rem auto; color: #18181b; line-height: 1.5; }
  pre { white-space: pre-wrap; font-family: ui-monospace, monospace; }
  h1 { font-size: 20px; }
</style></head><body>
<h1>%s</h1>
<pre>%s</pre>
<script>window.onload = () => window.print();</script>
</body></html>`, htmlEscape(pm.Title), htmlEscape(pm.Title), htmlEscape(pm.ContentMarkdown))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func slug(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "postmortem"
	}
	return out
}
