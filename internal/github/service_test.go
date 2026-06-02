package github

import "testing"

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
