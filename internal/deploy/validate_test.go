package deploy

import "testing"

func strp(s string) *string { return &s }

func TestIsNonPublicURL(t *testing.T) {
	nonPublic := []string{
		"http://localhost:8080/api/v1/github/webhook",
		"http://127.0.0.1:8080/hook",
		"http://192.168.1.10/hook",
		"http://10.0.0.5:3000/hook",
		"http://myhost.local/hook",
		"http://::not a url::",
	}
	for _, u := range nonPublic {
		if !isNonPublicURL(u) {
			t.Errorf("expected %q to be non-public", u)
		}
	}
	public := []string{
		"https://api.convdeploy.com/api/v1/github/webhook",
		"https://abc123.ngrok-free.app/api/v1/github/webhook",
		"http://93.184.216.34/hook",
	}
	for _, u := range public {
		if isNonPublicURL(u) {
			t.Errorf("expected %q to be public", u)
		}
	}
}

func TestValidateProjectInput(t *testing.T) {
	tests := []struct {
		name      string
		project   string
		owner     string
		repo      string
		branch    *string
		wantError bool
	}{
		{"valid minimal", "my-app", "octocat", "hello-world", nil, false},
		{"valid with branch", "My App 2.0", "org-name", "repo.name", strp("feat/login"), false},
		{"empty name", "", "octocat", "repo", nil, true},
		{"name too long", string(make([]byte, 80)), "octocat", "repo", nil, true},
		{"name leading dash", "-app", "octocat", "repo", nil, true},
		{"name with slash", "my/app", "octocat", "repo", nil, true},
		{"owner with slash", "app", "bad/owner", "repo", nil, true},
		{"owner path traversal", "app", "..", "repo", nil, true},
		{"repo with space", "app", "octocat", "bad repo", nil, true},
		{"branch with dotdot", "app", "octocat", "repo", strp("a..b"), true},
		{"branch with space", "app", "octocat", "repo", strp("bad branch"), true},
		{"empty branch ok", "app", "octocat", "repo", strp(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProjectInput(tt.project, tt.owner, tt.repo, tt.branch)
			if (err != nil) != tt.wantError {
				t.Errorf("validateProjectInput(%q, %q, %q) error = %v, wantError %v",
					tt.project, tt.owner, tt.repo, err, tt.wantError)
			}
		})
	}
}
