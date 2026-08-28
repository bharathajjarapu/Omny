package omny

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `
token: sekret
default: groq
providers:
  groq:
    url: https://api.groq.com/openai/v1
    rpd: 1000
    keys: [gsk_a, gsk_b]
  cerebras:
    url: https://api.cerebras.ai/v1
    keys: [csk_a]
aliases:
  fast: [cerebras/llama-3.3-70b, groq/llama-3.3-70b-versatile]
fallback: [fast]
`

func write(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "omny.yaml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name string
		body string
		mode os.FileMode
		want string
	}{
		{name: "valid", body: good, mode: 0o600},
		{
			name: "world readable",
			body: good, mode: 0o644,
			want: "0644",
		},
		{
			name: "group readable",
			body: good, mode: 0o640,
			want: "0640",
		},
		{
			name: "unknown field",
			body: good + "\nrobustness: high\n", mode: 0o600,
			want: "robustness",
		},
		{
			name: "no token",
			body: strings.Replace(good, "token: sekret", "", 1), mode: 0o600,
			want: "token",
		},
		{
			name: "wildcard bind",
			body: good + "\nlisten: 0.0.0.0:8080\n", mode: 0o600,
			want: "0.0.0.0",
		},
		{
			name: "wildcard bind v6",
			body: good + "\nlisten: \"[::]:8080\"\n", mode: 0o600,
			want: "::",
		},
		{
			name: "no providers",
			body: "token: t\ndefault: groq\n", mode: 0o600,
			want: "no providers",
		},
		{
			name: "every provider without keys",
			body: "token: t\ndefault: groq\nproviders:\n  groq:\n    url: https://x/v1\n    keys: []\n", mode: 0o600,
			want: "no provider has a key",
		},
		{
			name: "one provider without keys",
			body: strings.Replace(good, "aliases:", "  spare:\n    url: https://y/v1\naliases:", 1), mode: 0o600,
		},
		{
			name: "keyless arms an otherwise empty pool",
			body: "token: t\ndefault: groq\nproviders:\n  groq:\n    url: https://x/v1\n    keyless: true\n", mode: 0o600,
		},
		{
			name: "keyless and keyed at once",
			body: "token: t\ndefault: groq\nproviders:\n  groq:\n    url: https://x/v1\n    keyless: true\n    keys: [k]\n", mode: 0o600,
			want: "keyless and also has 1 keys",
		},
		{
			name: "negative rpm",
			body: strings.Replace(good, "rpd: 1000", "rpm: -1", 1), mode: 0o600,
			want: "rpm -1 is negative",
		},
		{
			name: "negative ttft",
			body: strings.Replace(good, "rpd: 1000", "ttft: -5s", 1), mode: 0o600,
			want: "ttft -5s is negative",
		},
		{
			name: "provider without url",
			body: "token: t\ndefault: groq\nproviders:\n  groq:\n    keys: [k]\n", mode: 0o600,
			want: "url",
		},
		{
			name: "alias names unknown provider",
			body: strings.Replace(good, "cerebras/llama-3.3-70b,", "cerbras/llama-3.3-70b,", 1), mode: 0o600,
			want: "cerbras",
		},
		{
			name: "alias entry has no model",
			body: strings.Replace(good, "cerebras/llama-3.3-70b,", "cerebras,", 1), mode: 0o600,
			want: "cerebras",
		},
		{
			name: "default names unknown provider",
			body: strings.Replace(good, "default: groq", "default: grok", 1), mode: 0o600,
			want: "grok",
		},
		{
			name: "fallback names unknown alias",
			body: strings.Replace(good, "fallback: [fast]", "fallback: [quick]", 1), mode: 0o600,
			want: "quick",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(write(t, tt.body, tt.mode))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("load: %v", err)
			case tt.want != "" && err == nil:
				t.Fatalf("loaded, want error naming %q", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	c, err := Load(write(t, good, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q, want the loopback default", c.Listen)
	}
	if c.State != "./omny.state.json" {
		t.Errorf("state = %q, want the default path", c.State)
	}
	if got := c.Providers["cerebras"].RPD; got != 0 {
		t.Errorf("absent rpd = %d, want 0 meaning unknown", got)
	}
	if got := len(c.Providers["groq"].Keys); got != 2 {
		t.Errorf("keys = %d, want 2", got)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("loaded a file that does not exist")
	}
}

func TestLoadCollects(t *testing.T) {
	t.Parallel()
	body := "token: \"\"\ndefault: nope\nproviders:\n  groq:\n    url: https://x/v1\n    keys: [k]\n"
	_, err := Load(write(t, body, 0o600))
	if err == nil {
		t.Fatal("loaded an invalid config")
	}
	for _, want := range []string{"token", "nope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
