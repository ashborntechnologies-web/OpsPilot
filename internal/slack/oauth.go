package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/crypto"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// OAuth scopes requested at install. incoming-webhook is included so the install also
// returns a default channel; chat:write + commands + channels:read power notifications,
// slash commands, and channel listing.
const oauthScopes = "chat:write,commands,channels:read,incoming-webhook"

// ─── Signed OAuth state ───────────────────────────────────────────────────────

func (s *Service) stateKey() string {
	if s.signingSecret != "" {
		return s.signingSecret
	}
	return s.encKey
}

// signState encodes orgID+userID with an HMAC so the public callback can trust which
// org/user initiated the install without a server-side session.
func (s *Service) signState(orgID, userID uuid.UUID) string {
	payload := orgID.String() + ":" + userID.String()
	mac := hmac.New(sha256.New, []byte(s.stateKey()))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) verifyState(state string) (orgID, userID uuid.UUID, ok bool) {
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		return uuid.UUID{}, uuid.UUID{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, false
	}
	mac := hmac.New(sha256.New, []byte(s.stateKey()))
	mac.Write(raw)
	if !hmac.Equal([]byte(parts[1]), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return uuid.UUID{}, uuid.UUID{}, false
	}
	ids := strings.SplitN(string(raw), ":", 2)
	if len(ids) != 2 {
		return uuid.UUID{}, uuid.UUID{}, false
	}
	o, err1 := uuid.Parse(ids[0])
	u, err2 := uuid.Parse(ids[1])
	if err1 != nil || err2 != nil {
		return uuid.UUID{}, uuid.UUID{}, false
	}
	return o, u, true
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

// HandleInstallURL returns the Slack OAuth authorize URL for the active org.
// GET /orgs/:orgId/slack/install — admin (RequireOrgMembership has set org context).
func (s *Service) HandleInstallURL(c *gin.Context) {
	if !s.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Slack is not configured on this server"})
		return
	}
	userID, _ := middleware.GetUserID(c)
	orgID, _ := middleware.GetOrgID(c)

	q := url.Values{}
	q.Set("client_id", s.clientID)
	q.Set("scope", oauthScopes)
	q.Set("redirect_uri", s.redirectURI)
	q.Set("state", s.signState(orgID, userID))
	c.JSON(http.StatusOK, gin.H{"url": "https://slack.com/oauth/v2/authorize?" + q.Encode()})
}

// HandleCallback completes the OAuth dance: exchange the code for a bot token and store
// it (encrypted). Public — Slack redirects the browser here; trust is the signed state.
// GET /slack/callback?code=&state=
func (s *Service) HandleCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")
	orgID, userID, ok := s.verifyState(state)
	if code == "" || !ok {
		c.Redirect(http.StatusFound, s.frontendURL+"/settings/integrations?slack=error")
		return
	}

	form := url.Values{}
	form.Set("client_id", s.clientID)
	form.Set("client_secret", s.clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", s.redirectURI)

	var res struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Access string `json:"access_token"` // bot token (xoxb-…)
		Team   struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"`
		IncomingWebhook struct {
			ChannelID string `json:"channel_id"`
			Channel   string `json:"channel"`
		} `json:"incoming_webhook"`
	}
	if err := s.postForm(c.Request.Context(), "oauth.v2.access", form, &res); err != nil || !res.OK || res.Access == "" {
		c.Redirect(http.StatusFound, s.frontendURL+"/settings/integrations?slack=error")
		return
	}

	enc, err := crypto.Encrypt(res.Access, s.encKey)
	if err != nil {
		c.Redirect(http.StatusFound, s.frontendURL+"/settings/integrations?slack=error")
		return
	}

	// The incoming-webhook channel is a sensible default for all three routes; the user
	// refines them on the integrations page.
	var defChID, defChName *string
	if res.IncomingWebhook.ChannelID != "" {
		defChID = &res.IncomingWebhook.ChannelID
		name := strings.TrimPrefix(res.IncomingWebhook.Channel, "#")
		defChName = &name
	}

	_, dberr := s.db.Pool.Exec(c.Request.Context(), `
		INSERT INTO slack_integrations
		    (org_id, team_id, workspace_name, bot_token, installed_by,
		     alert_channel_id, alert_channel_name, deploy_channel_id, deploy_channel_name,
		     summary_channel_id, summary_channel_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $6, $7, $6, $7)
		ON CONFLICT (org_id) DO UPDATE SET
		    team_id = EXCLUDED.team_id,
		    workspace_name = EXCLUDED.workspace_name,
		    bot_token = EXCLUDED.bot_token,
		    installed_by = EXCLUDED.installed_by,
		    updated_at = NOW()`,
		orgID, res.Team.ID, res.Team.Name, enc, userID, defChID, defChName)
	if dberr != nil {
		c.Redirect(http.StatusFound, s.frontendURL+"/settings/integrations?slack=error")
		return
	}
	c.Redirect(http.StatusFound, s.frontendURL+"/settings/integrations?slack=connected")
}

// HandleGetIntegration returns the org's Slack connection (token omitted by json:"-").
// GET /orgs/:orgId/slack — any member.
func (s *Service) HandleGetIntegration(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	in, err := s.loadIntegration(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load Slack integration"})
		return
	}
	if in == nil {
		c.JSON(http.StatusOK, gin.H{"connected": false, "configured": s.Enabled()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"connected": true, "configured": s.Enabled(), "integration": in})
}

// HandleListChannels lists the workspace's channels for the config dropdowns.
// GET /orgs/:orgId/slack/channels — any member.
func (s *Service) HandleListChannels(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	in, err := s.loadIntegration(c.Request.Context(), orgID)
	if err != nil || in == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Slack is not connected"})
		return
	}
	params := url.Values{}
	params.Set("exclude_archived", "true")
	params.Set("types", "public_channel,private_channel")
	params.Set("limit", "200")

	var res struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Channels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"channels"`
	}
	if err := s.apiGet(c.Request.Context(), in.BotToken, "conversations.list", params, &res); err != nil || !res.OK {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to list Slack channels"})
		return
	}
	c.JSON(http.StatusOK, res.Channels)
}

// HandleUpdateChannels saves channel routing. PATCH /orgs/:orgId/slack — admin.
func (s *Service) HandleUpdateChannels(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	var req struct {
		AlertChannelID     *string `json:"alert_channel_id"`
		AlertChannelName   *string `json:"alert_channel_name"`
		DeployChannelID    *string `json:"deploy_channel_id"`
		DeployChannelName  *string `json:"deploy_channel_name"`
		SummaryChannelID   *string `json:"summary_channel_id"`
		SummaryChannelName *string `json:"summary_channel_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tag, err := s.db.Pool.Exec(c.Request.Context(), `
		UPDATE slack_integrations SET
		    alert_channel_id = $2, alert_channel_name = $3,
		    deploy_channel_id = $4, deploy_channel_name = $5,
		    summary_channel_id = $6, summary_channel_name = $7,
		    updated_at = NOW()
		WHERE org_id = $1`,
		orgID, req.AlertChannelID, req.AlertChannelName, req.DeployChannelID,
		req.DeployChannelName, req.SummaryChannelID, req.SummaryChannelName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update channels"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Slack is not connected"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "channels updated"})
}

// HandleDisconnect removes the org's Slack integration. DELETE /orgs/:orgId/slack — admin.
func (s *Service) HandleDisconnect(c *gin.Context) {
	orgID, _ := middleware.GetOrgID(c)
	_, err := s.db.Pool.Exec(c.Request.Context(), `DELETE FROM slack_integrations WHERE org_id = $1`, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to disconnect Slack"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Slack disconnected"})
}

// postForm POSTs application/x-www-form-urlencoded (used by oauth.v2.access, which is not
// JSON/Bearer based).
func (s *Service) postForm(ctx context.Context, method string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}
