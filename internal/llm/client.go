// Package llm is the single Claude Messages API client for the platform.
// All AI call sites (intent classification, diagnosis, framework detection) go
// through Complete so timeout, retry, and model policy live in one place.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	apiURL     = "https://api.anthropic.com/v1/messages"
	apiVersion = "2023-06-01"

	// DefaultModel is used for all platform AI calls (classification, diagnosis,
	// framework detection). claude-sonnet-4-6 replaces the deprecated
	// claude-sonnet-4-20250514 (retires June 15, 2026).
	DefaultModel = "claude-sonnet-4-6"

	maxRetries = 2
)

// Client calls the Anthropic Messages API with timeouts and retry on
// transient errors (429 rate limit, 5xx, 529 overloaded).
type Client struct {
	apiKey     string
	model      string
	baseURL    string // overridable in tests
	httpClient *http.Client
}

// New returns a client using DefaultModel. An empty apiKey is allowed —
// Complete then fails fast with ErrNoAPIKey so callers can degrade gracefully.
func New(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		model:   DefaultModel,
		baseURL: apiURL,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// ErrNoAPIKey is returned when the client was constructed without an API key.
var ErrNoAPIKey = fmt.Errorf("ANTHROPIC_API_KEY not configured")

// APIError is a non-2xx response from the Anthropic API after retries.
type APIError struct {
	StatusCode int
	Type       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic API error (HTTP %d, %s): %s", e.StatusCode, e.Type, e.Message)
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type response struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends a single user message with a system prompt and returns the
// text of the first content block. Retries transient failures with backoff.
func (c *Client) Complete(ctx context.Context, system, userMessage string, maxTokens int) (string, error) {
	if c.apiKey == "" {
		return "", ErrNoAPIKey
	}

	body, err := json.Marshal(request{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []message{{Role: "user", Content: userMessage}},
	})
	if err != nil {
		return "", err
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff(attempt, lastErr)):
			}
		}

		text, err := c.doRequest(ctx, body)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return "", err
		}
	}
	return "", lastErr
}

func (c *Client) doRequest(ctx context.Context, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var parsed response
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		if resp.StatusCode != http.StatusOK {
			return "", &APIError{StatusCode: resp.StatusCode, Type: "unparseable", Message: http.StatusText(resp.StatusCode)}
		}
		return "", fmt.Errorf("failed to parse Anthropic response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{StatusCode: resp.StatusCode, Type: "api_error", Message: http.StatusText(resp.StatusCode)}
		if parsed.Error != nil {
			apiErr.Type = parsed.Error.Type
			apiErr.Message = parsed.Error.Message
		}
		// Carry Retry-After through for backoff.
		if ra := resp.Header.Get("retry-after"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 && secs <= 60 {
				apiErr.Message = fmt.Sprintf("%s (retry-after %ds)", apiErr.Message, secs)
			}
		}
		return "", apiErr
	}

	if len(parsed.Content) == 0 {
		return "", fmt.Errorf("empty response from Claude")
	}
	return parsed.Content[0].Text, nil
}

// isRetryable reports whether the error is a transient API condition
// (rate limit, server error, overloaded) or a network/transport failure.
func isRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// backoff returns the wait before the given retry attempt (1-based).
func backoff(attempt int, _ error) time.Duration {
	return time.Duration(attempt) * 2 * time.Second
}
