package aws

import (
	"strings"
	"testing"
)

func TestRenderBootstrapTemplate(t *testing.T) {
	const acct = "111122223333"
	const extID = "convdeploy-1234abcd-5678"

	out := renderBootstrapTemplate(acct, extID)

	if !strings.Contains(out, "sts:ExternalId: "+extID) {
		t.Errorf("rendered template missing external ID condition; got:\n%s", out)
	}
	if !strings.Contains(out, "arn:aws:iam::"+acct+":root") {
		t.Errorf("rendered template missing platform account principal")
	}
	if strings.Contains(out, externalIDPlaceholder) {
		t.Errorf("rendered template still contains the placeholder %q", externalIDPlaceholder)
	}
	// The Parameters block should be stripped now that values are hardcoded.
	if strings.Contains(out, "\nParameters:") {
		t.Errorf("rendered template should not contain a Parameters block")
	}
	// Sanity: the SSM permissions added in Phase 2A.2 are present.
	if !strings.Contains(out, "ssm:PutParameter") {
		t.Errorf("rendered template missing ssm:PutParameter permission")
	}
}

func TestNewExternalID(t *testing.T) {
	a := NewExternalID()
	b := NewExternalID()

	if !strings.HasPrefix(a, "convdeploy-") {
		t.Errorf("external ID should be prefixed convdeploy-, got %q", a)
	}
	if a == b {
		t.Errorf("external IDs should be unique, got duplicate %q", a)
	}
	if a == LegacyExternalID {
		t.Errorf("generated external ID must not equal the legacy shared value")
	}
}

func TestGithubTokenParamName(t *testing.T) {
	cases := []struct {
		project, sha, want string
	}{
		{"proj-1", "abcdef1234567890", "/convdeploy/proj-1/abcdef12/github-token"},
		{"p2", "short", "/convdeploy/p2/short/github-token"},
	}
	for _, c := range cases {
		if got := githubTokenParamName(c.project, c.sha); got != c.want {
			t.Errorf("githubTokenParamName(%q,%q) = %q, want %q", c.project, c.sha, got, c.want)
		}
	}
}
