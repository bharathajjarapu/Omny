package omny

import (
	"slices"
	"testing"
)

func routes(t *testing.T) *Config {
	t.Helper()
	c := &Config{
		Token: "t", Listen: "127.0.0.1:0", State: "s", Default: "groq",
		Providers: map[string]*Provider{
			"groq":       {URL: "https://groq.test/v1", Keys: []string{"a"}},
			"cerebras":   {URL: "https://cerebras.test/v1", Keys: []string{"b"}},
			"openrouter": {URL: "https://openrouter.test/v1", Keys: []string{"c"}},
		},
		Aliases: map[string][]string{
			"fast": {"cerebras/llama-3.3-70b", "groq/llama-3.3-70b-versatile"},
			"big":  {"openrouter/deepseek/deepseek-r1:free"},
		},
	}
	if err := c.check(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoute(t *testing.T) {
	c := routes(t)
	tests := []struct {
		name string
		in   string
		want []target
	}{
		{
			name: "alias keeps its written order",
			in:   "fast",
			want: []target{{prov: "cerebras", model: "llama-3.3-70b"}, {prov: "groq", model: "llama-3.3-70b-versatile"}},
		},
		{
			name: "explicit pin is exactly that target",
			in:   "groq/llama-3.3-70b-versatile",
			want: []target{{prov: "groq", model: "llama-3.3-70b-versatile"}},
		},
		{
			name: "model id containing a slash splits once",
			in:   "openrouter/deepseek/deepseek-r1:free",
			want: []target{{prov: "openrouter", model: "deepseek/deepseek-r1:free"}},
		},
		{
			name: "alias entry with a slashed model id survives",
			in:   "big",
			want: []target{{prov: "openrouter", model: "deepseek/deepseek-r1:free"}},
		},
		{
			name: "unrecognised name goes to the default, not to every provider",
			in:   "llama-3.3-70b-versatile",
			want: []target{{prov: "groq", model: "llama-3.3-70b-versatile"}},
		},
		{
			name: "pin naming an unconfigured provider is just an unknown name",
			in:   "grok/whatever",
			want: []target{{prov: "groq", model: "grok/whatever"}},
		},
		{
			name: "an openrouter id against a config without that provider reaches the default whole",
			in:   "deepseek/deepseek-r1:free",
			want: []target{{prov: "groq", model: "deepseek/deepseek-r1:free"}},
		},
		{
			name: "empty name resolves to nothing",
			in:   "",
			want: nil,
		},
		{
			name: "an alias shadows a same-named model",
			in:   "fast",
			want: []target{{prov: "cerebras", model: "llama-3.3-70b"}, {prov: "groq", model: "llama-3.3-70b-versatile"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := c.route(tt.in)
			named := func(a, b target) bool { return a.prov == b.prov && a.model == b.model }
			if !slices.EqualFunc(got, tt.want, named) {
				t.Errorf("route(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for _, g := range got {
				if g.at != c.Providers[g.prov] {
					t.Errorf("target %s carries %v, want the configured provider", g.prov, g.at)
				}
			}
		})
	}
}
