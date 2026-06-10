package webhooks

import (
	"net"
	"testing"
)

func TestValidateWebhookInput(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		events    []string
		wantError bool
	}{
		{"valid https", "https://example.com/hook", []string{"deploy.started"}, false},
		{"valid http", "http://example.com/hook", []string{"deploy.succeeded", "deploy.failed"}, false},
		{"bad scheme", "ftp://example.com/hook", []string{"deploy.started"}, true},
		{"no host", "https://", []string{"deploy.started"}, true},
		{"not a url", "not a url", []string{"deploy.started"}, true},
		{"unknown event", "https://example.com", []string{"bogus.event"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWebhookInput(tt.url, tt.events)
			if (err != nil) != tt.wantError {
				t.Errorf("validateWebhookInput(%q, %v) = %v, wantError %v", tt.url, tt.events, err, tt.wantError)
			}
		})
	}
}

func TestIsDisallowedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "169.254.169.254", "0.0.0.0", "::1"}
	for _, s := range blocked {
		if !isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be disallowed", s)
		}
	}
	allowed := []string{"93.184.216.34", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be allowed", s)
		}
	}
}
