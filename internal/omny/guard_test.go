package omny

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func logged(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(l), &d); err != nil {
			t.Fatalf("log line is not JSON: %s", l)
		}
		out = append(out, d)
	}
	return out
}

func TestPanicIsFiveHundredAndLoggedAtError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	srv, _ := stand(t, &Config{
		Default:   "p",
		Providers: map[string]*Provider{"p": {URL: "http://x/v1", Keys: []string{"k"}}},
	})
	srv.log = slog.New(slog.NewJSONHandler(&buf, nil))

	h := srv.shield(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("upstream ate my socks") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 — a panic must not reach the client as an EOF", rec.Code)
	}
	var e struct {
		Error struct{ Message, Type string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Message == "" {
		t.Errorf("body = %s, want one OpenAI-shaped error", rec.Body)
	}
	lines := logged(t, &buf)
	if len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Fatalf("want one ERROR line, got %v", lines)
	}
	if !strings.Contains(lines[0]["stack"].(string), "guard_test.go") {
		t.Error("the line carries no stack, which is the only thing that makes it actionable")
	}
	if lines[0]["panic"] != "upstream ate my socks" {
		t.Errorf("panic = %v, want the value that was raised", lines[0]["panic"])
	}
}

func TestDeliberateAbortIsNotCaught(t *testing.T) {
	t.Parallel()
	srv, _ := stand(t, &Config{
		Default:   "p",
		Providers: map[string]*Provider{"p": {URL: "http://x/v1", Keys: []string{"k"}}},
	})
	srv.log = slog.New(slog.DiscardHandler)
	defer func() {
		v := recover()
		if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want ErrAbortHandler to pass straight through", v)
		}
	}()
	h := srv.shield(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

func TestBodyCeilingShedsAndReleases(t *testing.T) {
	t.Parallel()
	up := fake(t, completion)
	srv, g := stand(t, &Config{
		Default:   "p",
		Providers: map[string]*Provider{"p": {URL: up.URL + "/v1", Keys: []string{"k"}}},
		Aliases:   map[string][]string{"fast": {"p/m"}},
	})
	body := `{"model":"fast","messages":[{"role":"user","content":"hello"}]}`

	res, _ := post(t, g, body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d under the default ceiling, want 200", res.StatusCode)
	}
	if n := srv.held.Load(); n != 0 {
		t.Errorf("held = %d after the request finished, want 0", n)
	}

	srv.hold = 4
	res, out := post(t, g, body)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d over the ceiling, want 503 (body %s)", res.StatusCode, out)
	}
	if !bytes.Contains(out, []byte("retry shortly")) {
		t.Errorf("body = %s, want it to say the wait is short", out)
	}
	if n := srv.held.Load(); n != 0 {
		t.Errorf("held = %d after a shed request, want 0", n)
	}
}

func TestReadyzAnswersWhatHealthzMustNot(t *testing.T) {
	t.Parallel()
	up := fake(t, boom(http.StatusUnauthorized))
	g := gateway(t, &Config{
		Default:   "p",
		Providers: map[string]*Provider{"p": {URL: up.URL + "/v1", Keys: []string{"k"}}},
		Aliases:   map[string][]string{"fast": {"p/m"}},
	})
	if res := ask(t, g, http.MethodGet, "/readyz", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d before anything failed, want 200", res.StatusCode)
	}
	post(t, g, `{"model":"fast","messages":[]}`)

	if res := ask(t, g, http.MethodGet, "/healthz", ""); res.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200: the process is fine and a restart would not help", res.StatusCode)
	}
	res := ask(t, g, http.MethodGet, "/readyz", "")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d with every key dead, want 503", res.StatusCode)
	}
	if res := ask(t, g, http.MethodGet, "/readyz", ""); res.StatusCode == http.StatusUnauthorized {
		t.Error("/readyz needs a token, but a supervisor probing it does not hold one")
	}
}

func TestOneIdTiesEveryLineOfARequestTogether(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	dead, live := fake(t, boom(http.StatusInternalServerError)), fake(t, completion)
	srv, g := stand(t, &Config{
		Default: "good",
		Providers: map[string]*Provider{
			"bad":  {URL: dead.URL + "/v1", Keys: []string{"kb"}},
			"good": {URL: live.URL + "/v1", Keys: []string{"kg"}},
		},
		Aliases: map[string][]string{"fast": {"bad/m", "good/m"}},
	})
	srv.log = slog.New(slog.NewJSONHandler(&buf, nil))

	res, _ := post(t, g, `{"model":"fast","messages":[]}`)
	if got := res.Header.Get("X-Request-Id"); got == "" {
		t.Error("no X-Request-Id came back, so the caller cannot quote one at a log")
	}
	lines := logged(t, &buf)
	var fail, done map[string]any
	for _, l := range lines {
		switch l["msg"] {
		case "attempt failed":
			fail = l
		case "request":
			done = l
		}
	}
	if fail == nil || done == nil {
		t.Fatalf("want both an attempt line and a request line, got %v", lines)
	}
	if fail["id"] == nil || fail["id"] != done["id"] {
		t.Errorf("attempt id %v and request id %v differ; the failure cannot be traced to its request",
			fail["id"], done["id"])
	}
	if done["id"] != res.Header.Get("X-Request-Id") {
		t.Errorf("logged id %v is not the one the caller was given (%s)", done["id"], res.Header.Get("X-Request-Id"))
	}
}

func TestCallerSuppliedIdIsHonoured(t *testing.T) {
	t.Parallel()
	srv, _ := stand(t, &Config{
		Default:   "p",
		Providers: map[string]*Provider{"p": {URL: "http://x/v1", Keys: []string{"k"}}},
	})
	for _, c := range []struct {
		name, send string
		mine       bool
	}{
		{"kept verbatim", "abc-123", false},
		{"absent means ours", "", true},
		{"a newline would forge a log line", "bad\nid", true},
		{"so would a carriage return", "bad\rid", true},
		{"and an unbounded one bloats every line it touches", strings.Repeat("x", 4000), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if c.send != "" {
				r.Header["X-Request-Id"] = []string{c.send}
			}
			got := srv.mark(r)
			switch {
			case got == "":
				t.Fatal("no id was produced at all")
			case len(got) > 64:
				t.Errorf("id is %d chars, want it bounded", len(got))
			case strings.ContainsAny(got, "\r\n"):
				t.Errorf("id %q carries a line break into the log", got)
			case c.mine && got == c.send:
				t.Errorf("id = %q, want one of ours rather than the caller's", got)
			case !c.mine && got != c.send:
				t.Errorf("id = %q, want the caller's own %q", got, c.send)
			}
		})
	}
}

func TestIdsDoNotRepeatAcrossRestarts(t *testing.T) {
	t.Parallel()
	cfg := func() *Config {
		return &Config{
			Default:   "p",
			Providers: map[string]*Provider{"p": {URL: "http://x/v1", Keys: []string{"k"}}},
		}
	}
	a, _ := stand(t, cfg())
	b, _ := stand(t, cfg())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	first, second := a.mark(r), b.mark(r)
	if first == second {
		t.Errorf("two runs both issued %q; a restart would make one log line ambiguous", first)
	}
	if !strings.HasSuffix(first, "-1") || !strings.HasSuffix(second, "-1") {
		t.Errorf("ids %q and %q should still count from one within their own run", first, second)
	}
	if next := a.mark(r); next == first {
		t.Errorf("the same run issued %q twice", next)
	}
}
