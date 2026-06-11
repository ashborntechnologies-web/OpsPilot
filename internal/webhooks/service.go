package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Service handles webhook CRUD and delivery.
type Service struct {
	db     *models.DB
	client *http.Client
}

// isDisallowedIP blocks loopback, private, link-local (incl. 169.254.169.254
// instance metadata), and unspecified addresses — webhook URLs are user-supplied,
// so delivery must never reach internal infrastructure (SSRF).
func isDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func NewService(db *models.DB) *Service {
	// The Control hook runs against the *resolved* address right before connect,
	// so a hostname that re-resolves to an internal IP (DNS rebinding) is still blocked.
	// ALLOW_PRIVATE_WEBHOOKS=true relaxes this for local development.
	allowPrivate := os.Getenv("ALLOW_PRIVATE_WEBHOOKS") == "true"
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || isDisallowedIP(ip) {
				return fmt.Errorf("webhook destination %s is not allowed", host)
			}
			return nil
		},
	}
	return &Service{
		db: db,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
			// Don't follow redirects — a permitted URL could redirect to an internal one.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// validWebhookEvents is the set of event types a webhook may subscribe to.
var validWebhookEvents = map[string]bool{
	models.WebhookEventDeployStarted:   true,
	models.WebhookEventDeploySucceeded: true,
	models.WebhookEventDeployFailed:    true,
}

// validateWebhookInput checks the URL scheme/host and the subscribed event types.
func validateWebhookInput(rawURL string, events []string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url must be a valid http(s) URL")
	}
	if len(rawURL) > 2048 {
		return fmt.Errorf("url too long")
	}
	for _, ev := range events {
		if !validWebhookEvents[ev] {
			return fmt.Errorf("unknown event type %q — valid: deploy.started, deploy.succeeded, deploy.failed", ev)
		}
	}
	return nil
}

// ---- HTTP handlers ----

func (s *Service) HandleList(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	rows, err := s.db.Pool.Query(c.Request.Context(),
		`SELECT id, project_id, url, events, active, created_at, updated_at
		 FROM webhooks WHERE project_id = $1 ORDER BY created_at DESC`, projectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list webhooks"})
		return
	}
	defer rows.Close()

	var hooks []models.Webhook
	for rows.Next() {
		var h models.Webhook
		if err := rows.Scan(&h.ID, &h.ProjectID, &h.URL, &h.Events, &h.Active, &h.CreatedAt, &h.UpdatedAt); err != nil {
			continue
		}
		hooks = append(hooks, h)
	}
	if hooks == nil {
		hooks = []models.Webhook{}
	}
	c.JSON(http.StatusOK, hooks)
}

func (s *Service) HandleCreate(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	var body struct {
		URL    string   `json:"url"`
		Secret string   `json:"secret"`
		Events []string `json:"events"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.URL == "" || len(body.Events) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url and at least one event are required"})
		return
	}
	if err := validateWebhookInput(body.URL, body.Events); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var h models.Webhook
	err = s.db.Pool.QueryRow(c.Request.Context(),
		`INSERT INTO webhooks (project_id, url, secret, events)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, project_id, url, events, active, created_at, updated_at`,
		projectID, body.URL, body.Secret, body.Events,
	).Scan(&h.ID, &h.ProjectID, &h.URL, &h.Events, &h.Active, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create webhook"})
		return
	}
	c.JSON(http.StatusCreated, h)
}

func (s *Service) HandleUpdate(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	webhookID, err := uuid.Parse(c.Param("webhookId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook id"})
		return
	}

	var body struct {
		URL    *string  `json:"url"`
		Secret *string  `json:"secret"`
		Events []string `json:"events"`
		Active *bool    `json:"active"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if body.URL != nil {
		if err := validateWebhookInput(*body.URL, nil); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if body.Events != nil {
		if err := validateWebhookInput("https://placeholder.invalid", body.Events); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	var h models.Webhook
	err = s.db.Pool.QueryRow(c.Request.Context(),
		`SELECT id, project_id, url, secret, events, active, created_at, updated_at
		 FROM webhooks WHERE id = $1 AND project_id = $2`, webhookID, projectID,
	).Scan(&h.ID, &h.ProjectID, &h.URL, &h.Secret, &h.Events, &h.Active, &h.CreatedAt, &h.UpdatedAt)
	if err == pgx.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch webhook"})
		return
	}

	if body.URL != nil {
		h.URL = *body.URL
	}
	if body.Secret != nil {
		h.Secret = *body.Secret
	}
	if body.Events != nil {
		h.Events = body.Events
	}
	if body.Active != nil {
		h.Active = *body.Active
	}

	err = s.db.Pool.QueryRow(c.Request.Context(),
		`UPDATE webhooks SET url=$1, secret=$2, events=$3, active=$4, updated_at=NOW()
		 WHERE id=$5 AND project_id=$6
		 RETURNING id, project_id, url, events, active, created_at, updated_at`,
		h.URL, h.Secret, h.Events, h.Active, webhookID, projectID,
	).Scan(&h.ID, &h.ProjectID, &h.URL, &h.Events, &h.Active, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update webhook"})
		return
	}
	c.JSON(http.StatusOK, h)
}

func (s *Service) HandleDelete(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	webhookID, err := uuid.Parse(c.Param("webhookId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook id"})
		return
	}

	result, err := s.db.Pool.Exec(c.Request.Context(),
		`DELETE FROM webhooks WHERE id = $1 AND project_id = $2`, webhookID, projectID,
	)
	if err != nil || result.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "webhook deleted"})
}

// ---- Delivery ----

// FireEvent delivers a webhook event to all active webhooks for the project that subscribe to eventType.
// Runs in a goroutine — caller does not block.
func (s *Service) FireEvent(projectID uuid.UUID, eventType string, payload map[string]any) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		rows, err := s.db.Pool.Query(ctx,
			`SELECT url, secret FROM webhooks
			 WHERE project_id = $1 AND active = true AND $2 = ANY(events)`,
			projectID, eventType,
		)
		if err != nil {
			return
		}
		defer rows.Close()

		body, _ := json.Marshal(map[string]any{
			"event":   eventType,
			"payload": payload,
		})

		for rows.Next() {
			var url, secret string
			if err := rows.Scan(&url, &secret); err != nil {
				continue
			}
			s.deliver(url, secret, body)
		}
	}()
}

func (s *Service) deliver(url, secret string, body []byte) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Info(fmt.Sprintf("[webhook] invalid URL %s: %v", url, err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ConvDeploy-Webhook/1.0")

	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-ConvDeploy-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := s.client.Do(req)
	if err != nil {
		slog.Error(fmt.Sprintf("[webhook] delivery failed to %s: %v", url, err))
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Info(fmt.Sprintf("[webhook] delivery to %s returned %d", url, resp.StatusCode))
	}
}

// BuildPayload constructs a standard webhook payload from deployment context.
func BuildPayload(projectID, projectName, environment, deploymentID, commitSHA, commitMsg string) map[string]any {
	p := map[string]any{
		"project_id":   projectID,
		"project_name": projectName,
		"environment":  environment,
		"timestamp":    fmt.Sprintf("%s", time.Now().UTC().Format(time.RFC3339)),
	}
	if deploymentID != "" {
		p["deployment_id"] = deploymentID
	}
	if commitSHA != "" {
		p["commit_sha"] = commitSHA
	}
	if commitMsg != "" {
		p["commit_message"] = commitMsg
	}
	return p
}
