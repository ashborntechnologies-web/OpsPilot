// Package slack integrates OpsPilot with Slack: alert/deploy notifications, a daily
// summary digest, and /opspilot slash commands. It talks to the Slack Web API over raw
// HTTP (no SDK) — chat.postMessage, oauth.v2.access, conversations.list — with Bearer
// token auth. One workspace per org; the bot token is encrypted at rest (pkg/crypto).
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/crypto"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

const slackAPIBase = "https://slack.com/api/"

// Deployer is the subset of the deploy service the slash commands need. Injected to
// avoid a slack↔deploy import cycle.
type Deployer interface {
	TriggerDeploy(ctx context.Context, projectID uuid.UUID, envName string) (string, error)
	TriggerRollback(ctx context.Context, projectID uuid.UUID, envName string) (string, error)
}

type Service struct {
	db            *models.DB
	httpClient    *http.Client
	encKey        string
	prevKey       string
	clientID      string
	clientSecret  string
	signingSecret string
	redirectURI   string // PUBLIC_API_URL + /api/v1/slack/callback
	frontendURL   string
	deployer      Deployer
}

func NewService(db *models.DB, encKey, prevKey, clientID, clientSecret, signingSecret, publicAPIURL, frontendURL string) *Service {
	return &Service{
		db:            db,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		encKey:        encKey,
		prevKey:       prevKey,
		clientID:      clientID,
		clientSecret:  clientSecret,
		signingSecret: signingSecret,
		redirectURI:   strings.TrimRight(publicAPIURL, "/") + "/api/v1/slack/callback",
		frontendURL:   strings.TrimRight(frontendURL, "/"),
	}
}

// SetDeployer wires the deploy service used by /opspilot deploy|rollback.
func (s *Service) SetDeployer(d Deployer) { s.deployer = d }

// Enabled reports whether Slack OAuth + signing are configured.
func (s *Service) Enabled() bool {
	return s.clientID != "" && s.clientSecret != "" && s.signingSecret != ""
}

// ─── Slack Web API (raw HTTP) ─────────────────────────────────────────────────

// SendMessage is the base post: it merges channelID into the provided message body
// (a JSON object with "blocks"/"attachments"/"text") and POSTs to chat.postMessage.
func (s *Service) SendMessage(ctx context.Context, token, channelID string, blocks []byte) error {
	body := map[string]any{}
	if len(blocks) > 0 {
		if err := json.Unmarshal(blocks, &body); err != nil {
			return fmt.Errorf("slack: invalid message body: %w", err)
		}
	}
	body["channel"] = channelID
	return s.apiPostOK(ctx, token, "chat.postMessage", body)
}

// apiPostOK POSTs a JSON payload and requires {"ok":true} in the response.
func (s *Service) apiPostOK(ctx context.Context, token, method string, payload any) error {
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.apiPost(ctx, token, method, payload, &res); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("slack %s failed: %s", method, res.Error)
	}
	return nil
}

func (s *Service) apiPost(ctx context.Context, token, method string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// apiGet performs a GET with Bearer auth and query params (for conversations.list).
func (s *Service) apiGet(ctx context.Context, token, method string, params url.Values, out any) error {
	u := slackAPIBase + method
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// ─── Integration storage ──────────────────────────────────────────────────────

// loadIntegration returns the org's Slack integration with the bot token decrypted, or
// (nil, nil) when the org has not connected Slack.
func (s *Service) loadIntegration(ctx context.Context, orgID uuid.UUID) (*models.SlackIntegration, error) {
	var in models.SlackIntegration
	err := s.db.Pool.QueryRow(ctx, `
		SELECT id, org_id, team_id, workspace_name, bot_token,
		       alert_channel_id, alert_channel_name, deploy_channel_id, deploy_channel_name,
		       summary_channel_id, summary_channel_name, installed_by, created_at, updated_at
		FROM slack_integrations WHERE org_id = $1`, orgID,
	).Scan(&in.ID, &in.OrgID, &in.TeamID, &in.WorkspaceName, &in.BotToken,
		&in.AlertChannelID, &in.AlertChannelName, &in.DeployChannelID, &in.DeployChannelName,
		&in.SummaryChannelID, &in.SummaryChannelName, &in.InstalledBy, &in.CreatedAt, &in.UpdatedAt)
	if err != nil {
		return nil, nil // not connected (or not found) — callers treat as "no Slack"
	}
	tok, err := crypto.Decrypt(in.BotToken, s.encKey, s.prevKey)
	if err != nil {
		return nil, fmt.Errorf("slack: decrypt bot token: %w", err)
	}
	in.BotToken = tok
	return &in, nil
}

// ─── Notifications ────────────────────────────────────────────────────────────

const (
	colorError   = "#b91c1c" // red-700
	colorWarn    = "#b45309" // amber-700
	colorSuccess = "#15803d" // green-700
)

// PostAlert posts a color-coded alert to the org's alert channel with links to the war
// room. Best-effort: a missing integration/channel is a silent no-op.
func (s *Service) PostAlert(ctx context.Context, orgID uuid.UUID, alert models.Alert, projectName, envName, incidentURL string) error {
	in, err := s.loadIntegration(ctx, orgID)
	if err != nil || in == nil || in.AlertChannelID == nil {
		return err
	}
	color := colorWarn
	if alert.Severity == models.SeverityError {
		color = colorError
	}
	attachment := map[string]any{
		"color": color,
		"blocks": []any{
			section(fmt.Sprintf("*:rotating_light: %s*\n%s", alert.Title, alert.Summary)),
			contextBlock(fmt.Sprintf("%s · %s", projectName, envName)),
			actions(
				urlButton("View War Room", incidentURL),
				urlButton("Acknowledge", incidentURL),
			),
		},
	}
	return s.postAttachments(ctx, in.BotToken, *in.AlertChannelID, attachment)
}

// PostDeployResult posts a deploy success/failure to the org's deploy channel.
func (s *Service) PostDeployResult(ctx context.Context, orgID uuid.UUID, projectName, envName, status, commitSHA, commitMessage, deployURL string) error {
	in, err := s.loadIntegration(ctx, orgID)
	if err != nil || in == nil || in.DeployChannelID == nil {
		return err
	}
	success := status == "live" || status == "succeeded"
	color := colorError
	headline := ":x: Deploy failed"
	if success {
		color = colorSuccess
		headline = ":white_check_mark: Deploy succeeded"
	}
	short := commitSHA
	if len(short) > 8 {
		short = short[:8]
	}
	msg := truncate(commitMessage, 72)
	text := fmt.Sprintf("*%s* — %s / %s\n`%s` %s", headline, projectName, envName, short, msg)
	attachment := map[string]any{
		"color":  color,
		"blocks": []any{section(text), actions(urlButton("View Deployment", deployURL))},
	}
	return s.postAttachments(ctx, in.BotToken, *in.DeployChannelID, attachment)
}

// PostDailySummary posts an AI-generated morning briefing (markdown) to the org's summary
// channel. The rich generation lives in internal/summary; this just delivers it. No-op if
// Slack isn't connected or no summary channel is set. Returns (delivered, error).
func (s *Service) PostDailySummary(ctx context.Context, orgID uuid.UUID, markdown string) (bool, error) {
	in, err := s.loadIntegration(ctx, orgID)
	if err != nil || in == nil || in.SummaryChannelID == nil {
		return false, err
	}
	body, _ := json.Marshal(map[string]any{"blocks": []any{section(markdown)}})
	if err := s.SendMessage(ctx, in.BotToken, *in.SummaryChannelID, body); err != nil {
		return false, err
	}
	return true, nil
}

// PostActionProposal posts a pending AI-action approval to the org's alert channel with a
// link to review it in OpsPilot. (Approve/Reject happen in-app, where the acting user's
// platform role can be verified — Slack users aren't mapped to platform users.)
func (s *Service) PostActionProposal(ctx context.Context, orgID uuid.UUID, action models.AIAction, reviewURL string) error {
	in, err := s.loadIntegration(ctx, orgID)
	if err != nil || in == nil || in.AlertChannelID == nil {
		return err
	}
	conf := ""
	if action.ConfidenceScore != nil {
		conf = fmt.Sprintf(" · %d%% confidence", int(*action.ConfidenceScore*100))
	}
	text := fmt.Sprintf("*:robot_face: AI proposed a %s action*%s\n%s", action.ActionType, conf, action.Rationale)
	attachment := map[string]any{
		"color":  colorWarn,
		"blocks": []any{section(text), actions(urlButton("Review & approve", reviewURL))},
	}
	return s.postAttachments(ctx, in.BotToken, *in.AlertChannelID, attachment)
}

// postAttachments posts a single color-coded attachment (chat.postMessage attachments).
func (s *Service) postAttachments(ctx context.Context, token, channelID string, attachment map[string]any) error {
	return s.apiPostOK(ctx, token, "chat.postMessage", map[string]any{
		"channel":     channelID,
		"attachments": []any{attachment},
	})
}

// ─── Block Kit helpers ────────────────────────────────────────────────────────

func section(markdown string) map[string]any {
	return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": markdown}}
}

func contextBlock(text string) map[string]any {
	return map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": text}}}
}

func actions(elements ...map[string]any) map[string]any {
	els := make([]any, len(elements))
	for i, e := range elements {
		els[i] = e
	}
	return map[string]any{"type": "actions", "elements": els}
}

func urlButton(text, link string) map[string]any {
	b := map[string]any{
		"type": "button",
		"text": map[string]any{"type": "plain_text", "text": text},
	}
	if link != "" {
		b["url"] = link
	}
	return b
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
