package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// verifySignature validates the X-Slack-Signature header (HMAC-SHA256 of
// "v0:<timestamp>:<body>" with the signing secret) and returns the raw request body.
// Rejects requests older than 5 minutes (replay protection).
func (s *Service) verifySignature(c *gin.Context) ([]byte, bool) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, false
	}
	ts := c.GetHeader("X-Slack-Request-Timestamp")
	sig := c.GetHeader("X-Slack-Signature")
	if ts == "" || sig == "" || s.signingSecret == "" {
		return nil, false
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(tsInt, 0)) > 5*time.Minute {
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(s.signingSecret))
	fmt.Fprintf(mac, "v0:%s:%s", ts, string(body))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return nil, false
	}
	return body, true
}

// orgForTeam maps a Slack workspace (team_id) to an OpsPilot org.
func (s *Service) orgForTeam(ctx context.Context, teamID string) (uuid.UUID, string, bool) {
	var orgID uuid.UUID
	var token string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT org_id, bot_token FROM slack_integrations WHERE team_id = $1`, teamID,
	).Scan(&orgID, &token)
	if err != nil {
		return uuid.UUID{}, "", false
	}
	return orgID, token, true
}

// HandleCommand processes /opspilot slash commands. POST /slack/commands (signed).
// All responses are ephemeral except deploy/rollback confirmations (in_channel).
func (s *Service) HandleCommand(c *gin.Context) {
	body, ok := s.verifySignature(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}
	form, _ := url.ParseQuery(string(body))
	teamID := form.Get("team_id")
	text := strings.TrimSpace(form.Get("text"))

	orgID, _, found := s.orgForTeam(c.Request.Context(), teamID)
	if !found {
		c.JSON(http.StatusOK, ephemeral("This Slack workspace isn't linked to an OpsPilot workspace. Connect it in Settings → Integrations."))
		return
	}

	fields := strings.Fields(text)
	cmd := ""
	if len(fields) > 0 {
		cmd = strings.ToLower(fields[0])
	}

	switch cmd {
	case "status":
		c.JSON(http.StatusOK, ephemeral(s.statusText(c.Request.Context(), orgID)))
	case "incidents":
		c.JSON(http.StatusOK, ephemeral(s.incidentsText(c.Request.Context(), orgID)))
	case "deploy", "rollback":
		c.JSON(http.StatusOK, s.confirmAction(c.Request.Context(), orgID, cmd, fields[1:]))
	default:
		c.JSON(http.StatusOK, ephemeral(helpText()))
	}
}

// statusText summarizes the org's environments and recent deploys from the DB (fast — no
// AWS calls, so it stays within Slack's 3s response budget).
func (s *Service) statusText(ctx context.Context, orgID uuid.UUID) string {
	var ready, total, openInc int
	_ = s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE e.stack_status='ready'), COUNT(*)
		FROM environments e JOIN projects p ON p.id = e.project_id
		WHERE p.org_id = $1 AND e.is_preview = false`, orgID).Scan(&ready, &total)
	_ = s.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM incidents WHERE org_id = $1 AND status <> 'resolved'`, orgID).Scan(&openInc)

	var b strings.Builder
	fmt.Fprintf(&b, "*OpsPilot status*\n• Environments ready: %d/%d\n• Open incidents: %d\n", ready, total, openInc)

	rows, err := s.db.Pool.Query(ctx, `
		SELECT p.name, d.status, d.updated_at
		FROM deployments d JOIN projects p ON p.id = d.project_id
		WHERE p.org_id = $1
		ORDER BY d.updated_at DESC LIMIT 5`, orgID)
	if err == nil {
		defer rows.Close()
		b.WriteString("• Recent deploys:\n")
		any := false
		for rows.Next() {
			var name, status string
			var ts time.Time
			if rows.Scan(&name, &status, &ts) == nil {
				any = true
				fmt.Fprintf(&b, "   – %s: %s (%s)\n", name, status, ts.UTC().Format("Jan 2 15:04"))
			}
		}
		if !any {
			b.WriteString("   – none yet\n")
		}
	}
	return b.String()
}

func (s *Service) incidentsText(ctx context.Context, orgID uuid.UUID) string {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, COALESCE(title,'Incident'), severity, status FROM incidents
		WHERE org_id = $1 AND status <> 'resolved' ORDER BY created_at DESC LIMIT 10`, orgID)
	if err != nil {
		return "Couldn't load incidents."
	}
	defer rows.Close()
	var b strings.Builder
	b.WriteString("*Open incidents*\n")
	any := false
	for rows.Next() {
		var id uuid.UUID
		var title, sev, status string
		if rows.Scan(&id, &title, &sev, &status) == nil {
			any = true
			fmt.Fprintf(&b, "• [%s] <%s/incidents/%s|%s> — %s\n", sev, s.frontendURL, id, title, status)
		}
	}
	if !any {
		return "No open incidents. :white_check_mark:"
	}
	return b.String()
}

// confirmAction posts an in-channel confirmation with an Approve button for deploy/rollback.
func (s *Service) confirmAction(ctx context.Context, orgID uuid.UUID, action string, args []string) gin.H {
	if len(args) == 0 {
		return ephemeral(fmt.Sprintf("Usage: `/opspilot %s [project] %s`", action, ifDeploy(action, "[env]", "")))
	}
	projectName := args[0]
	env := "production"
	if action == "deploy" && len(args) > 1 {
		env = strings.ToLower(args[1])
	}

	var projectID uuid.UUID
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id FROM projects WHERE org_id = $1 AND lower(name) = lower($2) LIMIT 1`, orgID, projectName,
	).Scan(&projectID)
	if err != nil {
		return ephemeral(fmt.Sprintf("Project %q not found in this workspace.", projectName))
	}

	value, _ := json.Marshal(map[string]string{"project_id": projectID.String(), "env": env, "action": action})
	verb := "Deploy"
	prompt := fmt.Sprintf("Deploy *%s* to *%s*?", projectName, env)
	if action == "rollback" {
		verb = "Roll back"
		prompt = fmt.Sprintf("Roll back *%s* (production)?", projectName)
	}

	return gin.H{
		"response_type": "in_channel",
		"blocks": []any{
			section(prompt),
			map[string]any{
				"type": "actions",
				"elements": []any{
					map[string]any{
						"type":      "button",
						"style":     "primary",
						"text":      map[string]any{"type": "plain_text", "text": verb},
						"action_id": "confirm_" + action,
						"value":     string(value),
					},
					map[string]any{
						"type":      "button",
						"text":      map[string]any{"type": "plain_text", "text": "Cancel"},
						"action_id": "cancel",
						"value":     "cancel",
					},
				},
			},
		},
	}
}

// HandleInteractivity processes Block Kit button clicks (deploy/rollback approvals).
// POST /slack/interactivity (signed).
func (s *Service) HandleInteractivity(c *gin.Context) {
	body, ok := s.verifySignature(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}
	form, _ := url.ParseQuery(string(body))
	var payload struct {
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(form.Get("payload")), &payload); err != nil || len(payload.Actions) == 0 {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	act := payload.Actions[0]

	if act.ActionID == "cancel" {
		c.JSON(http.StatusOK, gin.H{"replace_original": true, "text": "Cancelled."})
		return
	}
	if act.ActionID != "confirm_deploy" && act.ActionID != "confirm_rollback" {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	if s.deployer == nil {
		c.JSON(http.StatusOK, gin.H{"replace_original": true, "text": "Deploys are not available."})
		return
	}

	var v struct {
		ProjectID string `json:"project_id"`
		Env       string `json:"env"`
		Action    string `json:"action"`
	}
	if err := json.Unmarshal([]byte(act.Value), &v); err != nil {
		c.JSON(http.StatusOK, gin.H{"replace_original": true, "text": "Invalid action."})
		return
	}
	projectID, err := uuid.Parse(v.ProjectID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"replace_original": true, "text": "Invalid project."})
		return
	}

	// Run the workflow async — Slack expects a response within 3s.
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var msg string
		var e error
		if v.Action == "rollback" {
			msg, e = s.deployer.TriggerRollback(bg, projectID, v.Env)
		} else {
			msg, e = s.deployer.TriggerDeploy(bg, projectID, v.Env)
		}
		if e != nil {
			s.respondURL(bg, form.Get("response_url"), gin.H{"replace_original": false, "text": "Failed: " + e.Error()})
			return
		}
		s.respondURL(bg, form.Get("response_url"), gin.H{"replace_original": false, "text": ":white_check_mark: " + msg})
	}()

	c.JSON(http.StatusOK, gin.H{"replace_original": true, "text": fmt.Sprintf(":hourglass_flowing_sand: Starting %s…", v.Action)})
}

// respondURL posts a follow-up message to a Slack response_url (no auth needed).
func (s *Service) respondURL(ctx context.Context, responseURL string, payload gin.H) {
	if responseURL == "" {
		return
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, strings.NewReader(string(b)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func ephemeral(text string) gin.H {
	return gin.H{"response_type": "ephemeral", "text": text}
}

func ifDeploy(action, deployVal, other string) string {
	if action == "deploy" {
		return deployVal
	}
	return other
}

func helpText() string {
	return "*OpsPilot commands*\n" +
		"• `/opspilot status` — environments, incidents, recent deploys\n" +
		"• `/opspilot incidents` — open incidents\n" +
		"• `/opspilot deploy [project] [env]` — deploy (with confirmation)\n" +
		"• `/opspilot rollback [project]` — roll back production (with confirmation)"
}
