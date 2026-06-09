package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func signPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func TestVerifyWebhookSignature_Valid(t *testing.T) {
	secret := "mysecret"
	payload := []byte(`{"action":"opened"}`)
	sig := signPayload(payload, secret)
	assert.True(t, VerifyWebhookSignature(payload, "sha256="+sig, secret))
}

func TestVerifyWebhookSignature_WrongSecret(t *testing.T) {
	payload := []byte(`{"action":"opened"}`)
	sig := signPayload(payload, "correct-secret")
	assert.False(t, VerifyWebhookSignature(payload, "sha256="+sig, "wrong-secret"))
}

func TestVerifyWebhookSignature_TamperedPayload(t *testing.T) {
	secret := "mysecret"
	sig := signPayload([]byte(`{"action":"opened"}`), secret)
	assert.False(t, VerifyWebhookSignature([]byte(`{"action":"closed"}`), "sha256="+sig, secret))
}

func TestVerifyWebhookSignature_MissingPrefix(t *testing.T) {
	secret := "mysecret"
	payload := []byte(`{}`)
	sig := signPayload(payload, secret)
	// Without the "sha256=" prefix it should reject
	assert.False(t, VerifyWebhookSignature(payload, sig, secret))
}

func TestVerifyWebhookSignature_EmptySignature(t *testing.T) {
	assert.False(t, VerifyWebhookSignature([]byte(`{}`), "", "secret"))
}

func TestStripJSONFences(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", `{"framework":"go"}`, `{"framework":"go"}`},
		{"json fence", "```json\n{\"framework\":\"go\"}\n```", `{"framework":"go"}`},
		{"bare fence", "```\n{\"framework\":\"vite\"}\n```", `{"framework":"vite"}`},
		{"leading/trailing space", "  {\"a\":1}  ", `{"a":1}`},
		{"fence with surrounding whitespace", "  ```json\n{\"a\":1}\n```  ", `{"a":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripJSONFences(c.in); got != c.want {
				t.Errorf("stripJSONFences(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
