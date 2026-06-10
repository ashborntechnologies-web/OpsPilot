package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(serverURL string) *Client {
	c := New("test-key")
	c.baseURL = serverURL
	return c
}

func TestCompleteSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing x-api-key header")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version header")
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"hello"}]}`))
	}))
	defer srv.Close()

	got, err := testClient(srv.URL).Complete(context.Background(), "sys", "msg", 100)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestCompleteNoAPIKey(t *testing.T) {
	_, err := New("").Complete(context.Background(), "sys", "msg", 100)
	if !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("got %v, want ErrNoAPIKey", err)
	}
}

func TestCompleteRetriesOn529(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(529)
			w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"recovered"}]}`))
	}))
	defer srv.Close()

	got, err := testClient(srv.URL).Complete(context.Background(), "sys", "msg", 100)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "recovered" {
		t.Errorf("got %q, want %q", got, "recovered")
	}
	if calls.Load() != 2 {
		t.Errorf("got %d calls, want 2", calls.Load())
	}
}

func TestCompleteNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Complete(context.Background(), "sys", "msg", 100)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("got status %d, want 400", apiErr.StatusCode)
	}
	if calls.Load() != 1 {
		t.Errorf("got %d calls, want 1 (no retry on 4xx)", calls.Load())
	}
}

func TestCompleteRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := testClient(srv.URL).Complete(ctx, "sys", "msg", 100)
	if err == nil {
		t.Fatal("expected error")
	}
}
