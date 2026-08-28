package omny

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestWhoNamesTheCaller(t *testing.T) {
	c := &Config{Token: "solo", Tokens: map[string]string{"laptop": "aaa", "phone": "bbb"}}
	for _, x := range []struct{ tok, want string }{
		{"solo", "-"},
		{"aaa", "laptop"},
		{"bbb", "phone"},
		{"nope", ""},
		{"", ""},
	} {
		if got := c.who(x.tok); got != x.want {
			t.Errorf("who(%q) = %q, want %q", x.tok, got, x.want)
		}
	}
}

func TestAnUnsetTokenIsNeverAMatch(t *testing.T) {
	c := &Config{Tokens: map[string]string{"named": "aaa", "broken": ""}}
	if got := c.who(""); got != "" {
		t.Errorf("the empty token matched %q, want no match", got)
	}
	c = &Config{Token: "solo"}
	if got := c.who(""); got != "" {
		t.Errorf("the empty token matched %q, want no match", got)
	}
}

func TestTokenConfigIsRefused(t *testing.T) {
	for _, x := range []struct {
		name, want string
		c          Config
	}{
		{"none at all", "no token", Config{}},
		{"a named one left empty", `token "phone" is empty`, Config{Tokens: map[string]string{"phone": ""}}},
	} {
		t.Run(x.name, func(t *testing.T) {
			x.c.Listen, x.c.State, x.c.Default = "127.0.0.1:0", "s.json", "p"
			x.c.Providers = map[string]*Provider{"p": {URL: "http://x.invalid/v1", Keys: []string{"k"}}}
			err := x.c.check()
			if err == nil || !strings.Contains(err.Error(), x.want) {
				t.Errorf("check() = %v, want it to mention %q", err, x.want)
			}
		})
	}
}

func TestTokensAloneIsEnough(t *testing.T) {
	c := Config{
		Listen: "127.0.0.1:0", State: "s.json", Default: "p",
		Tokens:    map[string]string{"laptop": "aaa"},
		Providers: map[string]*Provider{"p": {URL: "http://x.invalid/v1", Keys: []string{"k"}}},
	}
	if err := c.check(); err != nil {
		t.Fatalf("tokens: alone was refused: %v", err)
	}
}

func TestEachNamedTokenOpensTheDoor(t *testing.T) {
	up := fake(t, completion)
	c := &Config{
		Default:   "groq",
		Tokens:    map[string]string{"laptop": "aaa", "phone": "bbb"},
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	}
	g := gateway(t, c)
	for _, x := range []struct {
		auth string
		want int
	}{
		{"Bearer aaa", 200}, {"Bearer bbb", 200}, {"Bearer " + tok, 200}, {"Bearer ccc", 401},
	} {
		res := hit(t, g, http.MethodPost, "/v1/chat/completions", x.auth, `{"model":"m","messages":[]}`)
		if res.StatusCode != x.want {
			t.Errorf("%s: status %d, want %d, body %s", x.auth, res.StatusCode, x.want, read(t, res))
		}
	}
}

func TestTheClientNameReachesTheLogLine(t *testing.T) {
	up := fake(t, completion)
	srv, g := stand(t, &Config{
		Default:   "groq",
		Tokens:    map[string]string{"phone": "bbb"},
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	var buf bytes.Buffer
	srv.log = slog.New(slog.NewJSONHandler(&buf, nil))

	hit(t, g, http.MethodPost, "/v1/chat/completions", "Bearer bbb", `{"model":"m","messages":[]}`)
	if got := buf.String(); !strings.Contains(got, `"client":"phone"`) {
		t.Errorf("no client on the request line: %s", got)
	}
}
