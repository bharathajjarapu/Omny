package omny

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type seen struct {
	model string
	auth  string
	path  string
	body  []byte
	head  http.Header
}

type upstream struct {
	*httptest.Server
	mu  sync.Mutex
	got []seen
}

func fake(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read: %v", err)
			return
		}
		asked, _ := peek(body)
		u.mu.Lock()
		u.got = append(u.got, seen{asked.Model, r.Header.Get("Authorization"), r.URL.Path, body, r.Header.Clone()})
		u.mu.Unlock()

		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) calls() []seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seen(nil), u.got...)
}

func completion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Remaining-Requests", "998")
	io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"llama-3.3-70b-versatile",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
}

func gateway(t *testing.T, c *Config) *httptest.Server {
	t.Helper()
	_, g := stand(t, c)
	return g
}

func stand(t *testing.T, c *Config) (*server, *httptest.Server) {
	t.Helper()
	c.Token, c.Listen, c.State = tok, "127.0.0.1:0", filepath.Join(t.TempDir(), "omny.state.json")
	if err := c.check(); err != nil {
		t.Fatal(err)
	}
	srv := serve(c)
	srv.log = slog.New(slog.DiscardHandler)
	g := httptest.NewServer(srv.mux())
	t.Cleanup(g.Close)
	return srv, g
}

const tok = "t0ken"

func hit(t *testing.T, g *httptest.Server, method, path, auth, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, g.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func ask(t *testing.T, g *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	return hit(t, g, method, path, "Bearer "+tok, body)
}

func read(t *testing.T, res *http.Response) []byte {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func post(t *testing.T, g *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	res := ask(t, g, http.MethodPost, "/v1/chat/completions", body)
	return res, read(t, res)
}

func TestChat(t *testing.T) {
	t.Parallel()
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default: "groq",
		Providers: map[string]*Provider{
			"groq": {URL: up.URL + "/v1", Keys: []string{"gsk_a"}},
		},
		Aliases: map[string][]string{"fast": {"groq/llama-3.3-70b-versatile"}},
	})

	res, out := post(t, g, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, out)
	}
	if !bytes.Contains(out, []byte(`"content":"hi"`)) {
		t.Errorf("body = %s, want the provider's completion", out)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	if got := res.Header.Get("X-RateLimit-Remaining-Requests"); got != "998" {
		t.Errorf("rate-limit header = %q, want it forwarded", got)
	}

	calls := up.calls()
	if len(calls) != 1 {
		t.Fatalf("upstream saw %d calls, want 1", len(calls))
	}
	if calls[0].model != "llama-3.3-70b-versatile" {
		t.Errorf("upstream model = %q, want the alias resolved to the provider's name", calls[0].model)
	}
	if calls[0].auth != "Bearer gsk_a" {
		t.Errorf("upstream auth = %q, want the pooled key", calls[0].auth)
	}
	if calls[0].path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", calls[0].path)
	}
}

func TestChatPassthrough(t *testing.T) {
	t.Parallel()
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
		Aliases:   map[string][]string{"fast": {"groq/real-model"}},
	})

	post(t, g, rich)

	got := up.calls()[0].body
	for _, want := range []string{"0.30000000000000004", "12345678901234567890", "a < b && c > d"} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("upstream lost %q from the body", want)
		}
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(rich), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	delete(before, "model")
	delete(after, "model")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("body changed beyond the model name:\n got %#v\nwant %#v", after, before)
	}
}

func TestChatResolution(t *testing.T) {
	t.Parallel()
	groq := fake(t, completion)
	cerebras := fake(t, completion)
	conf := func() *Config {
		return &Config{
			Default: "groq",
			Providers: map[string]*Provider{
				"groq":     {URL: groq.URL + "/v1", Keys: []string{"gsk_a"}},
				"cerebras": {URL: cerebras.URL + "/v1", Keys: []string{"csk_a"}},
			},
			Aliases: map[string][]string{"fast": {"cerebras/llama-3.3-70b"}},
		}
	}

	tests := []struct {
		name  string
		model string
		want  *upstream
		as    string
	}{
		{name: "alias picks its first target", model: "fast", want: cerebras, as: "llama-3.3-70b"},
		{name: "explicit pin", model: "groq/qwen3-32b", want: groq, as: "qwen3-32b"},
		{name: "unrecognised name goes to the default", model: "some-new-model", want: groq, as: "some-new-model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.want.calls())
			g := gateway(t, conf())
			res, out := post(t, g, `{"model":"`+tt.model+`","messages":[]}`)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body %s", res.StatusCode, out)
			}
			calls := tt.want.calls()
			if len(calls) != before+1 {
				t.Fatalf("provider saw %d calls, want one more than %d", len(calls), before)
			}
			if got := calls[len(calls)-1].model; got != tt.as {
				t.Errorf("upstream model = %q, want %q", got, tt.as)
			}
		})
	}
}

func TestChatRotates(t *testing.T) {
	t.Parallel()
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k1", "k2"}}},
	})

	for range 3 {
		post(t, g, `{"model":"m","messages":[]}`)
	}
	calls := up.calls()
	want := []string{"Bearer k1", "Bearer k2", "Bearer k1"}
	for i, w := range want {
		if calls[i].auth != w {
			t.Errorf("call %d used %q, want %q", i, calls[i].auth, w)
		}
	}
}

func TestChatProviderHeaders(t *testing.T) {
	t.Parallel()
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default: "openrouter",
		Providers: map[string]*Provider{"openrouter": {
			URL:     up.URL + "/v1",
			Keys:    []string{"k"},
			Headers: map[string]string{"HTTP-Referer": "https://example.test/omny"},
		}},
	})

	post(t, g, `{"model":"m","messages":[]}`)
	if got := up.calls()[0].head.Get("HTTP-Referer"); got != "https://example.test/omny" {
		t.Errorf("provider header = %q, want it sent verbatim", got)
	}
}

func TestChatRejects(t *testing.T) {
	t.Parallel()
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})

	tests := []struct {
		name string
		body string
		code int
	}{
		{name: "empty model", body: `{"messages":[]}`, code: 400},
		{name: "not JSON", body: `{"model":`, code: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, out := post(t, g, tt.body)
			if res.StatusCode != tt.code {
				t.Fatalf("status = %d, want %d (body %s)", res.StatusCode, tt.code, out)
			}
			var e struct {
				Error struct{ Message, Type string }
			}
			if err := json.Unmarshal(out, &e); err != nil || e.Error.Message == "" {
				t.Errorf("error body = %s, want an OpenAI-shaped error", out)
			}
		})
	}
}

func boom(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, `{"error":{"message":"upstream is having a day"}}`)
	}
}

func duo(t *testing.T, first, second string) *httptest.Server {
	t.Helper()
	return gateway(t, &Config{
		Default: "groq",
		Providers: map[string]*Provider{
			"cerebras": {URL: first + "/v1", Keys: []string{"c1"}},
			"groq":     {URL: second + "/v1", Keys: []string{"g1"}},
		},
		Aliases: map[string][]string{"fast": {"cerebras/m", "groq/m"}},
	})
}

func pair(t *testing.T, first http.HandlerFunc, keys ...string) (*upstream, *upstream, *httptest.Server) {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{"k1"}
	}
	bad, good := fake(t, first), fake(t, completion)
	g := gateway(t, &Config{
		Default: "groq",
		Providers: map[string]*Provider{
			"cerebras": {URL: bad.URL + "/v1", Keys: keys},
			"groq":     {URL: good.URL + "/v1", Keys: []string{"gsk_a"}},
		},
		Aliases: map[string][]string{"fast": {"cerebras/llama-3.3-70b", "groq/llama-3.3-70b-versatile"}},
	})
	return bad, good, g
}

func TestFailover(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		first http.HandlerFunc
	}{
		{name: "server error", first: boom(500)},
		{name: "bad gateway", first: boom(502)},
		{name: "rate limited", first: boom(429)},
		{name: "credentials rejected", first: boom(401)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad, good, g := pair(t, tt.first)

			res, out := post(t, g, `{"model":"fast","messages":[]}`)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want the second provider's 200 (body %s)", res.StatusCode, out)
			}
			if !bytes.Contains(out, []byte(`"content":"hi"`)) {
				t.Errorf("body = %s, want the second provider's completion", out)
			}
			if bytes.Contains(out, []byte("having a day")) {
				t.Error("the first provider's error reached the client")
			}
			if n := len(bad.calls()); n != 1 {
				t.Errorf("broken provider saw %d calls, want 1", n)
			}
			if n := len(good.calls()); n != 1 {
				t.Errorf("healthy provider saw %d calls, want 1", n)
			}
		})
	}
}

func TestFailoverUnreachable(t *testing.T) {
	t.Parallel()
	dead := httptest.NewServer(http.HandlerFunc(completion))
	dead.Close()
	good := fake(t, completion)
	g := duo(t, dead.URL, good.URL)

	res, out := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the reachable provider (body %s)", res.StatusCode, out)
	}
}

func TestFailoverOneKeyPerProvider(t *testing.T) {
	t.Parallel()
	bad, good, g := pair(t, boom(500), "c1", "c2", "c3")

	res, _ := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if n := len(bad.calls()); n != 1 {
		t.Errorf("broken provider saw %d calls in one request, want 1 — its keys were drained", n)
	}
	if n := len(good.calls()); n != 1 {
		t.Errorf("healthy provider saw %d calls, want 1", n)
	}
}

func TestFailoverBenches(t *testing.T) {
	t.Parallel()
	bad, _, g := pair(t, boom(500), "c1", "c2")

	post(t, g, `{"model":"fast","messages":[]}`)
	post(t, g, `{"model":"fast","messages":[]}`)

	calls := bad.calls()
	if len(calls) != 2 {
		t.Fatalf("broken provider saw %d calls across two requests, want 2", len(calls))
	}
	if calls[0].auth == calls[1].auth {
		t.Errorf("both requests used %s, want the benched key skipped", calls[0].auth)
	}
}

func TestFailoverBenchesEveryKey(t *testing.T) {
	t.Parallel()
	limited := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	bad, _, g := pair(t, limited, "c1", "c2")

	post(t, g, `{"model":"fast","messages":[]}`)
	post(t, g, `{"model":"fast","messages":[]}`)
	post(t, g, `{"model":"fast","messages":[]}`)

	if n := len(bad.calls()); n != 2 {
		t.Errorf("rate-limited provider saw %d calls, want 2 — both keys held for the hour it asked for", n)
	}
}

func TestFailoverExhausted(t *testing.T) {
	t.Parallel()
	one, two := fake(t, boom(500)), fake(t, boom(503))
	g := duo(t, one.URL, two.URL)

	res, out := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", res.StatusCode, out)
	}
	var e struct {
		Error struct{ Message, Type string }
	}
	if err := json.Unmarshal(out, &e); err != nil || e.Error.Message == "" {
		t.Fatalf("body = %s, want one OpenAI-shaped error", out)
	}
	if bytes.Contains(out, []byte("having a day")) {
		t.Error("an upstream error body reached the client")
	}
}

func token(s string) string {
	return `{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"` + s + `"}}]}`
}

func sse(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for _, f := range frames {
			io.WriteString(w, "data: "+f+"\n\n")
			rc.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		rc.Flush()
	}
}

func stream(t *testing.T, g *httptest.Server, model string) (*http.Response, string) {
	t.Helper()
	res, out := post(t, g, `{"model":"`+model+`","stream":true,"messages":[]}`)
	return res, string(out)
}

func TestStream(t *testing.T) {
	t.Parallel()
	up := fake(t, sse(token("Hel"), token("lo"), token("!")))
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})

	res, out := stream(t, g, "m")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, out)
	}
	if got := res.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content-type = %q, want the provider's", got)
	}
	for i, want := range []string{"Hel", "lo", "!", "[DONE]"} {
		at := strings.Index(out, want)
		if at < 0 {
			t.Fatalf("frame %d (%q) never reached the client: %s", i, want, out)
		}
		if n := strings.Count(out, want); n != 1 {
			t.Errorf("frame %q appeared %d times, want 1", want, n)
		}
	}
	if strings.Index(out, "Hel") > strings.Index(out, "lo") {
		t.Errorf("frames arrived out of order: %s", out)
	}
}

func TestStreamErrorFirstFrame(t *testing.T) {
	t.Parallel()
	poison := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: "+`{"error":{"message":"free tier exhausted"}}`+"\n\n")
	}
	bad, good := fake(t, poison), fake(t, sse(token("hi")))
	g := duo(t, bad.URL, good.URL)

	res, out := stream(t, g, "fast")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want a clean 200 from the second provider (body %s)", res.StatusCode, out)
	}
	if strings.Contains(out, "free tier exhausted") {
		t.Error("the poisoned frame reached the client — the gate committed on the status line")
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, "[DONE]") {
		t.Errorf("client did not get the second provider's clean stream: %s", out)
	}
	if n := len(good.calls()); n != 1 {
		t.Errorf("healthy provider saw %d calls, want 1", n)
	}
}

func TestStreamErrorInBody(t *testing.T) {
	t.Parallel()
	poison := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
	}
	bad, good, g := pair(t, poison)

	res, out := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK || !bytes.Contains(out, []byte(`"content":"hi"`)) {
		t.Fatalf("status = %d body = %s, want the second provider's completion", res.StatusCode, out)
	}
	if bytes.Contains(out, []byte("quota exceeded")) {
		t.Error("the 200-with-an-error body reached the client")
	}
	if len(bad.calls()) != 1 || len(good.calls()) != 1 {
		t.Errorf("calls: bad %d, good %d, want 1 each", len(bad.calls()), len(good.calls()))
	}
}

func TestStreamTruncatedAfterCommit(t *testing.T) {
	t.Parallel()
	cut := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		io.WriteString(w, "data: "+token("partial")+"\n\n")
		rc.Flush()
		panic(http.ErrAbortHandler)
	}
	dying, rescue := fake(t, cut), fake(t, sse(token("rescued")))
	g := duo(t, dying.URL, rescue.URL)

	res, out := stream(t, g, "fast")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the committed 200", res.StatusCode)
	}
	if !strings.Contains(out, "partial") {
		t.Errorf("committed content was lost: %s", out)
	}
	if strings.Contains(out, "rescued") {
		t.Error("failed over after commit — the client got two answers spliced together")
	}
	if strings.Contains(out, "omny_error") {
		t.Error("an error body was appended to a committed stream")
	}
	if n := len(rescue.calls()); n != 0 {
		t.Errorf("second provider saw %d calls, want 0 — nothing may be retried after commit", n)
	}
}

func TestStreamProgressive(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		io.WriteString(w, "data: "+token("t1")+"\n\n")
		rc.Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		io.WriteString(w, "data: "+token("t2")+"\n\ndata: [DONE]\n\n")
		rc.Flush()
	})
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})

	res := ask(t, g, http.MethodPost, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`)

	br := bufio.NewReader(res.Body)
	early := make(chan string, 1)
	go func() {
		line, _ := br.ReadString('\n')
		early <- line
	}()

	select {
	case line := <-early:
		if !strings.Contains(line, "t1") {
			t.Errorf("first line = %q, want the first token", line)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first token did not arrive while the provider still held the second")
	}

	close(release)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "t2") {
		t.Errorf("remainder = %q, want the second token", rest)
	}
}

func TestStreamEmpty(t *testing.T) {
	t.Parallel()
	empty := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}
	bad, good := fake(t, empty), fake(t, sse(token("hi")))
	g := duo(t, bad.URL, good.URL)

	res, out := stream(t, g, "fast")
	if res.StatusCode != http.StatusOK || !strings.Contains(out, "hi") {
		t.Fatalf("status = %d body = %s, want failover to the second provider", res.StatusCode, out)
	}
	if len(bad.calls()) != 1 || len(good.calls()) != 1 {
		t.Errorf("calls: bad %d, good %d, want 1 each", len(bad.calls()), len(good.calls()))
	}
}

func TestChatUnknownProviderGoesToDefault(t *testing.T) {
	t.Parallel()
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "openrouter",
		Providers: map[string]*Provider{"openrouter": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})

	res, out := post(t, g, `{"model":"deepseek/deepseek-r1:free","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the default provider to serve it (body %s)", res.StatusCode, out)
	}
	if got := up.calls()[0].model; got != "deepseek/deepseek-r1:free" {
		t.Errorf("upstream model = %q, want the name passed through whole", got)
	}
}

func TestChatClientFault(t *testing.T) {
	t.Parallel()
	bad, good, g := pair(t, boom(400))

	res, out := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream 400 forwarded (body %s)", res.StatusCode, out)
	}
	if n := len(good.calls()); n != 0 {
		t.Errorf("second provider saw %d calls, want 0 — a malformed request fails the same way everywhere", n)
	}
	if n := len(bad.calls()); n != 1 {
		t.Errorf("first provider saw %d calls, want 1", n)
	}
}

func TestChatOneProviderPerRequest(t *testing.T) {
	t.Parallel()
	up := fake(t, boom(500))
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k1", "k2"}}},
		Aliases:   map[string][]string{"big": {"groq/model-a", "groq/model-b"}},
	})

	res, _ := post(t, g, `{"model":"big","messages":[]}`)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
	if n := len(up.calls()); n != 1 {
		t.Errorf("provider saw %d calls in one request, want 1 — its keys were drained across targets", n)
	}
}

func TestChatNonJSONBody(t *testing.T) {
	t.Parallel()
	html := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
	}
	bad, good, g := pair(t, html)

	res, out := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK || !bytes.Contains(out, []byte(`"content":"hi"`)) {
		t.Fatalf("status = %d body = %s, want failover to the second provider", res.StatusCode, out)
	}
	if bytes.Contains(out, []byte("Bad Gateway")) {
		t.Error("an HTML error page was relayed as a completion")
	}
	if len(bad.calls()) != 1 || len(good.calls()) != 1 {
		t.Errorf("calls: bad %d, good %d, want 1 each", len(bad.calls()), len(good.calls()))
	}
}

func TestStreamNeverEnds(t *testing.T) {
	t.Parallel()
	flood := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		junk := bytes.Repeat([]byte("x"), 8<<10)
		for range 64 {
			w.Write(junk)
			rc.Flush()
		}
	}
	bad, good := fake(t, flood), fake(t, sse(token("hi")))
	g := duo(t, bad.URL, good.URL)

	res, out := stream(t, g, "fast")
	if res.StatusCode != http.StatusOK || !strings.Contains(out, "hi") {
		t.Fatalf("status = %d body = %.80s, want the gate to give up and fail over", res.StatusCode, out)
	}
}

func TestStreamClientLeaves(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		io.WriteString(w, "data: "+token("t1")+"\n\n")
		rc.Flush()
		select {
		case <-hold:
		case <-time.After(5 * time.Second):
		}
		for range 200 {
			io.WriteString(w, "data: "+token("more")+"\n\n")
			rc.Flush()
		}
	})
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"only"}}},
	})

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(res.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	res.Body.Close()
	close(hold)

	deadline := time.Now().Add(3 * time.Second)
	for {
		r2, out := post(t, g, `{"model":"m","stream":true,"messages":[]}`)
		if r2.StatusCode == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %d (%s) — the client leaving benched the provider's key", r2.StatusCode, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func slow(d time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		completion(w, r)
	}
}

func mute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	http.NewResponseController(w).Flush()
	<-r.Context().Done()
}

func TestTTFTFailsOver(t *testing.T) {
	stalled, healthy := fake(t, mute), fake(t, sse(token("hi")))
	srv, g := stand(t, &Config{
		Default: "groq",
		Aliases: map[string][]string{"fast": {"groq/a", "cerebras/b"}},
		Providers: map[string]*Provider{
			"groq":     {URL: stalled.URL + "/v1", Keys: []string{"k1"}},
			"cerebras": {URL: healthy.URL + "/v1", Keys: []string{"k2"}},
		},
	})
	srv.ttft, srv.idle = 60*time.Millisecond, time.Hour

	res := ask(t, g, http.MethodPost, "/v1/chat/completions", `{"model":"fast","stream":true,"messages":[]}`)
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "hi") {
		t.Fatalf("status %d, body %q — the healthy provider did not serve", res.StatusCode, body)
	}
	if n := len(healthy.calls()); n != 1 {
		t.Errorf("the healthy provider was called %d times, want 1", n)
	}
	if d := srv.pool.soonest([]target{{prov: "groq", model: "a"}}); d <= 0 {
		t.Error("the stalled provider's key was not benched")
	}
}

func TestNonStreamBudgetFailsOver(t *testing.T) {
	stalled, healthy := fake(t, slow(time.Minute)), fake(t, completion)
	srv, g := stand(t, &Config{
		Default: "groq",
		Aliases: map[string][]string{"fast": {"groq/a", "cerebras/b"}},
		Providers: map[string]*Provider{
			"groq":     {URL: stalled.URL + "/v1", Keys: []string{"k1"}},
			"cerebras": {URL: healthy.URL + "/v1", Keys: []string{"k2"}},
		},
	})
	srv.ttft, srv.idle = time.Hour, 60*time.Millisecond

	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	if n := len(healthy.calls()); n != 1 {
		t.Errorf("the healthy provider was called %d times, want 1", n)
	}
}

func TestIdleClosesStream(t *testing.T) {
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		io.WriteString(w, "data: "+token("t1")+"\n\n")
		rc.Flush()
		<-r.Context().Done()
	})
	srv, g := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	srv.idle = 60 * time.Millisecond

	done := make(chan []byte, 1)
	go func() {
		res := ask(t, g, http.MethodPost, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`)
		b, _ := io.ReadAll(res.Body)
		done <- b
	}()
	select {
	case b := <-done:
		if !strings.Contains(string(b), "t1") {
			t.Errorf("the committed frame was lost: %q", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the idle timer never fired; the client is hanging on a dead stream")
	}
}

func TestSlowSteadyStreamRunsOn(t *testing.T) {
	const chunks = 12
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for i := range chunks {
			io.WriteString(w, "data: "+token(fmt.Sprintf("t%d", i))+"\n\n")
			rc.Flush()
			time.Sleep(15 * time.Millisecond)
		}
		io.WriteString(w, "data: [DONE]\n\n")
		rc.Flush()
	})
	srv, g := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	srv.ttft, srv.idle = 30*time.Millisecond, 50*time.Millisecond

	res := ask(t, g, http.MethodPost, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`)
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.Count(string(b), "data: {"); got != chunks {
		t.Errorf("got %d chunks, want %d — a steady generation was cut off: %q", got, chunks, b)
	}
	if !strings.Contains(string(b), "[DONE]") {
		t.Error("the stream did not run to completion")
	}
}

func TestNoTotalCap(t *testing.T) {
	srv, _ := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: "http://x/v1", Keys: []string{"k"}}},
	})
	l := srv.listener()
	if l.WriteTimeout != 0 {
		t.Errorf("WriteTimeout is %v; it caps total stream duration, which nothing may", l.WriteTimeout)
	}
	if l.IdleTimeout != 0 || srv.up.Timeout != 0 {
		t.Errorf("a total timeout crept in: server idle %v, upstream client %v", l.IdleTimeout, srv.up.Timeout)
	}
}

func flaky(reset string) http.HandlerFunc {
	var n atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Reset", reset)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		completion(w, r)
	}
}

func chain(t *testing.T, first http.HandlerFunc, fallback ...string) (*upstream, *upstream, *server, *httptest.Server) {
	t.Helper()
	a, b := fake(t, first), fake(t, completion)
	srv, g := stand(t, &Config{
		Default:  "groq",
		Fallback: fallback,
		Aliases: map[string][]string{
			"fast": {"groq/a"},
			"big":  {"cerebras/b"},
		},
		Providers: map[string]*Provider{
			"groq":     {URL: a.URL + "/v1", Keys: []string{"k1"}},
			"cerebras": {URL: b.URL + "/v1", Keys: []string{"k2"}},
		},
	})
	return a, b, srv, g
}

func TestPauseCatchesRecovery(t *testing.T) {
	a, b, srv, g := chain(t, flaky("60ms"), "big")
	srv.pause = time.Second

	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	if n := len(a.calls()); n != 2 {
		t.Errorf("the requested alias was attempted %d times, want 2 — the pause must re-walk it", n)
	}
	if n := len(b.calls()); n != 0 {
		t.Errorf("the fallback alias was used %d times; the pause should have made it unnecessary", n)
	}
}

func TestPauseSkippedWhenNothingRecoversSoon(t *testing.T) {
	a, b, srv, g := chain(t, flaky("30s"), "big")
	srv.pause = 5 * time.Second

	start := time.Now()
	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the request took %v; the pause should have been skipped entirely", took)
	}
	if n := len(a.calls()); n != 1 {
		t.Errorf("the exhausted alias was re-walked %d times, want 1", n)
	}
	if n := len(b.calls()); n != 1 {
		t.Errorf("the fallback served %d times, want 1", n)
	}
}

func TestFallbackIsOptIn(t *testing.T) {
	_, b, _, g := chain(t, boom(500))

	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503, body %s", res.StatusCode, body)
	}
	if n := len(b.calls()); n != 0 {
		t.Errorf("an unconfigured fallback served %d requests; that is a surprise substitution", n)
	}
}

func TestFallbackSkipsWhatWasTried(t *testing.T) {
	a, b, _, g := chain(t, boom(500), "fast", "big")

	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	if n := len(a.calls()); n != 1 {
		t.Errorf("the requested alias was walked %d times, want 1", n)
	}
	if n := len(b.calls()); n != 1 {
		t.Errorf("the fallback served %d times, want 1", n)
	}
}

func limited(reset string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", reset)
		w.WriteHeader(http.StatusTooManyRequests)
	}
}

func TestPauseHappensOnce(t *testing.T) {
	a, b, c := fake(t, limited("80ms")), fake(t, limited("80ms")), fake(t, limited("80ms"))
	srv, g := stand(t, &Config{
		Default:  "groq",
		Fallback: []string{"big", "huge"},
		Aliases: map[string][]string{
			"fast": {"groq/a"},
			"big":  {"cerebras/b"},
			"huge": {"deepseek/c"},
		},
		Providers: map[string]*Provider{
			"groq":     {URL: a.URL + "/v1", Keys: []string{"k1"}},
			"cerebras": {URL: b.URL + "/v1", Keys: []string{"k2"}},
			"deepseek": {URL: c.URL + "/v1", Keys: []string{"k3"}},
		},
	})
	srv.pause = time.Second

	start := time.Now()
	res, _ := post(t, g, `{"model":"fast","messages":[]}`)
	took := time.Since(start)

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", res.StatusCode)
	}
	if n := len(a.calls()); n != 2 {
		t.Errorf("the requested alias was attempted %d times, want 2: the one pause re-walks it", n)
	}
	for name, u := range map[string]*upstream{"big": b, "huge": c} {
		if n := len(u.calls()); n != 1 {
			t.Errorf("fallback alias %s was attempted %d times, want 1 — it must not get a pause", name, n)
		}
	}
	if took > 400*time.Millisecond {
		t.Errorf("the request took %v; one pause of 80ms is due, not three", took)
	}
}

func TestExhaustedNamesTheWait(t *testing.T) {
	a, _, srv, g := chain(t, boom(429))
	srv.pause = time.Millisecond

	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", res.StatusCode)
	}
	var v struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("503 body is not JSON: %s", body)
	}
	if !strings.Contains(v.Error.Message, "soonest retry in 1m") {
		t.Errorf("message %q does not name the ladder's first rung", v.Error.Message)
	}
	if n := len(a.calls()); n != 1 {
		t.Errorf("the provider was hit %d times, want 1", n)
	}
}

func TestAuth(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})

	for _, c := range []struct {
		name string
		auth string
		want int
	}{
		{"the configured token", "Bearer " + tok, 200},
		{"a wrong token", "Bearer nope", 401},
		{"no header at all", "", 401},
		{"a bearer-less header", tok, 401},
		{"another scheme", "Basic " + tok, 401},
		{"the prefix and nothing else", "Bearer ", 401},
		{"the right token, wrong case scheme", "bearer " + tok, 401},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := hit(t, g, http.MethodPost, "/v1/chat/completions", c.auth, `{"model":"m","messages":[]}`)
			body := read(t, res)
			if res.StatusCode != c.want {
				t.Errorf("status %d, want %d, body %s", res.StatusCode, c.want, body)
			}
		})
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path+" needs the token", func(t *testing.T) {
			if res := hit(t, g, http.MethodGet, path, "", ""); res.StatusCode != http.StatusUnauthorized {
				t.Errorf("no token: status %d, want 401", res.StatusCode)
			}
			res := hit(t, g, http.MethodGet, path, "Bearer "+tok, "")
			if res.StatusCode != http.StatusOK {
				t.Errorf("with the token: status %d, want 200, body %s", res.StatusCode, read(t, res))
			}
		})
	}
}

func TestModels(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default: "groq",
		Aliases: map[string][]string{"fast": {"groq/a", "groq/b"}},
		Providers: map[string]*Provider{
			"groq": {URL: up.URL + "/v1", Keys: []string{"k"}},
		},
	})
	res := ask(t, g, http.MethodGet, "/v1/models", "")
	body := read(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	var v struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range v.Data {
		ids = append(ids, m.ID)
	}
	if want := "fast groq/a groq/b"; strings.Join(ids, " ") != want {
		t.Errorf("models = %q, want %q", strings.Join(ids, " "), want)
	}
	if len(up.calls()) != 0 {
		t.Error("the listing made a network call; it is served from config, never fetched")
	}
}

func TestStatus(t *testing.T) {
	up := fake(t, boom(429))
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", RPD: 50, Keys: []string{"sk-live-secret"}}},
	})
	post(t, g, `{"model":"m","messages":[]}`)

	res := ask(t, g, http.MethodGet, "/status", "")
	body := read(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	if strings.Contains(string(body), "sk-live-secret") {
		t.Fatalf("/status leaked the key: %s", body)
	}
	var got []card
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d cards, want 1: %s", len(got), body)
	}
	if got[0].Till == "" {
		t.Error("the benched key does not report when it comes back")
	}
	if got[0].Left == nil || *got[0].Left != 50 {
		t.Errorf("left = %v, want 50 — a failed attempt spends no quota", got[0].Left)
	}
	if got[0].Errs != 1 {
		t.Errorf("errs = %d, want 1", got[0].Errs)
	}
}

func TestStatusNeedsToken(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	for _, path := range []string{"/status", "/v1/models"} {
		res := hit(t, g, http.MethodGet, path, "", "")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token returned %d, want 401", path, res.StatusCode)
		}
	}
}

func TestExhaustedNamesRetryAfter(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		reply http.HandlerFunc
		want  bool
	}{
		{"a benched key recovers, so say when", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		}, true},
		{"a dead key never recovers, so say nothing", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			up := fake(t, c.reply)
			g := gateway(t, &Config{
				Default:   "p",
				Providers: map[string]*Provider{"p": {URL: up.URL + "/v1", Keys: []string{"k"}}},
				Aliases:   map[string][]string{"fast": {"p/m"}},
			})
			post(t, g, `{"model":"fast","messages":[]}`)
			res, out := post(t, g, `{"model":"fast","messages":[]}`)
			if res.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %s)", res.StatusCode, out)
			}
			got := res.Header.Get("Retry-After")
			if !c.want {
				if got != "" {
					t.Errorf("Retry-After = %q, want none: nothing recovers soon", got)
				}
				return
			}
			n, err := strconv.Atoi(got)
			if err != nil {
				t.Fatalf("Retry-After = %q, want whole seconds", got)
			}
			if n <= 0 || n > 3600 {
				t.Errorf("Retry-After = %d, want the hour the provider asked for", n)
			}
			if !bytes.Contains(out, []byte("soonest retry in")) {
				t.Errorf("body = %s, want it to name the same wait the header does", out)
			}
		})
	}
}
