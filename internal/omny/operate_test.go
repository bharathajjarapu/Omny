package omny

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func file(t *testing.T, dir string, c *Config) string {
	t.Helper()
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "omny.yaml")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func base(dir string) *Config {
	return &Config{Listen: "127.0.0.1:0", Token: tok, State: filepath.Join(dir, "omny.state.json")}
}

func solo(dir, url string) *Config {
	c := base(dir)
	c.Default = "one"
	c.Providers = map[string]*Provider{"one": {URL: url + "/v1", Keys: []string{"k-one"}}}
	c.Aliases = map[string][]string{"fast": {"one/m"}}
	return c
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 300 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func held(release <-chan struct{}, frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		io.WriteString(w, "data: "+frames[0]+"\n\n")
		rc.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		for _, f := range frames[1:] {
			io.WriteString(w, "data: "+f+"\n\n")
			rc.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		rc.Flush()
	}
}

func TestReloadAddsProvider(t *testing.T) {
	dead, live := fake(t, boom(500)), fake(t, completion)
	dir := t.TempDir()
	srv, g := stand(t, solo(dir, dead.URL))

	if res, _ := post(t, g, `{"model":"fast","messages":[]}`); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("before reload = %d, want 503 with only a broken provider", res.StatusCode)
	}

	n := solo(dir, dead.URL)
	n.Providers["two"] = &Provider{URL: live.URL + "/v1", Keys: []string{"k-two"}}
	n.Aliases["fast"] = []string{"one/m", "two/m"}
	if err := srv.reload(file(t, dir, n)); err != nil {
		t.Fatal(err)
	}

	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("after reload = %d %s, want the added provider to serve", res.StatusCode, body)
	}
	if len(live.calls()) != 1 {
		t.Fatalf("added provider saw %d calls, want 1", len(live.calls()))
	}
}

func TestReloadRejectsBadConfig(t *testing.T) {
	up := fake(t, completion)
	dir := t.TempDir()
	srv, g := stand(t, solo(dir, up.URL))

	p := filepath.Join(dir, "omny.yaml")
	if err := os.WriteFile(p, []byte("listen: \"0.0.0.0:8080\"\nproviders: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.reload(p); err == nil {
		t.Fatal("a config with no token, no providers and a wildcard bind was accepted")
	}
	if res, _ := post(t, g, `{"model":"fast","messages":[]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("after a rejected reload = %d, want the running config still serving", res.StatusCode)
	}
}

func TestReloadKeepsKeyState(t *testing.T) {
	up := fake(t, completion)
	dir := t.TempDir()
	srv, g := stand(t, solo(dir, up.URL))

	post(t, g, `{"model":"fast","messages":[]}`)
	k := srv.pool.keys["one"][0]
	srv.pool.fail(k, 429, time.Hour)

	n := solo(dir, up.URL)
	n.Providers["two"] = &Provider{URL: up.URL + "/v1", Keys: []string{"k-two"}}
	if err := srv.reload(file(t, dir, n)); err != nil {
		t.Fatal(err)
	}

	k = srv.pool.keys["one"][0]
	if k.reqs != 1 {
		t.Errorf("reqs = %d, want the day's count carried across the reload", k.reqs)
	}
	if srv.pool.free(k) {
		t.Error("the key came back available: a reload must not clear a live cooldown")
	}
}

func TestReloadShrinksKeys(t *testing.T) {
	up := fake(t, completion)
	dir := t.TempDir()
	c := solo(dir, up.URL)
	c.Providers["one"].Keys = []string{"k1", "k2", "k3"}
	srv, g := stand(t, c)

	for range 3 {
		post(t, g, `{"model":"fast","messages":[]}`)
	}

	n := solo(dir, up.URL)
	n.Providers["one"].Keys = []string{"k1"}
	if err := srv.reload(file(t, dir, n)); err != nil {
		t.Fatal(err)
	}
	if res, _ := post(t, g, `{"model":"fast","messages":[]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("after shrinking the key list = %d, want 200", res.StatusCode)
	}
}

func TestReloadRotatesToken(t *testing.T) {
	up := fake(t, completion)
	dir := t.TempDir()
	srv, g := stand(t, solo(dir, up.URL))

	n := solo(dir, up.URL)
	n.Token = "rotated"
	if err := srv.reload(file(t, dir, n)); err != nil {
		t.Fatal(err)
	}
	if res := hit(t, g, http.MethodPost, "/v1/chat/completions", "Bearer "+tok, `{"model":"fast","messages":[]}`); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("old token = %d, want 401 after a rotation", res.StatusCode)
	}
	if res := hit(t, g, http.MethodPost, "/v1/chat/completions", "Bearer rotated", `{"model":"fast","messages":[]}`); res.StatusCode != http.StatusOK {
		t.Errorf("new token = %d, want 200", res.StatusCode)
	}
}

func TestInflightKeepsItsConfig(t *testing.T) {
	release := make(chan struct{})
	first := fake(t, held(release, token("a"), token("b")))
	other := fake(t, completion)
	dir := t.TempDir()
	srv, g := stand(t, solo(dir, first.URL))

	body := make(chan string, 1)
	go func() {
		_, out := stream(t, g, "fast")
		body <- out
	}()
	until(t, "the stream to reach the provider", func() bool { return len(first.calls()) == 1 })

	n := solo(dir, other.URL)
	if err := srv.reload(file(t, dir, n)); err != nil {
		t.Fatal(err)
	}
	close(release)

	out := <-body
	for _, want := range []string{`"a"`, `"b"`, "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream lost %s across the reload:\n%s", want, out)
		}
	}
	if len(other.calls()) != 0 {
		t.Error("the in-flight request was re-routed by a reload")
	}
}

func TestInflightKeepsItsRoutes(t *testing.T) {
	release := make(chan struct{})
	slow := fake(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		boom(500)(w, r)
	})
	backup, elsewhere := fake(t, completion), fake(t, completion)
	dir := t.TempDir()
	c := base(dir)
	c.Default = "one"
	c.Providers = map[string]*Provider{
		"one":   {URL: slow.URL + "/v1", Keys: []string{"k-one"}},
		"two":   {URL: backup.URL + "/v1", Keys: []string{"k-two"}},
		"three": {URL: elsewhere.URL + "/v1", Keys: []string{"k-three"}},
	}
	c.Aliases = map[string][]string{"fast": {"one/m"}, "spare": {"two/m"}}
	c.Fallback = []string{"spare"}
	srv, g := stand(t, c)

	code := make(chan int, 1)
	go func() {
		res, _ := post(t, g, `{"model":"fast","messages":[]}`)
		code <- res.StatusCode
	}()
	until(t, "the first hop to reach its provider", func() bool { return len(slow.calls()) == 1 })

	n := base(dir)
	n.Default = "one"
	n.Providers = c.Providers
	n.Aliases = map[string][]string{"fast": {"one/m"}, "spare": {"three/m"}}
	n.Fallback = []string{"spare"}
	if err := srv.reload(file(t, dir, n)); err != nil {
		t.Fatal(err)
	}
	close(release)

	if got := <-code; got != http.StatusOK {
		t.Fatalf("status = %d, want the fallback the request started with to serve", got)
	}
	if len(elsewhere.calls()) != 0 {
		t.Error("a reload re-routed a request that was already walking its chain")
	}
	if len(backup.calls()) != 1 {
		t.Errorf("the original fallback saw %d calls, want 1", len(backup.calls()))
	}
}

type sink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *sink) lines(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func (s *sink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *sink) find(t *testing.T, msg string) map[string]any {
	t.Helper()
	for _, m := range s.lines(t) {
		if m["msg"] == msg {
			return m
		}
	}
	t.Fatalf("no %q line in:\n%s", msg, s.String())
	return nil
}

func heard(t *testing.T, c *Config) (*sink, *server, *httptest.Server) {
	t.Helper()
	out := &sink{}
	srv, g := stand(t, c)
	srv.log = slog.New(slog.NewJSONHandler(out, nil))
	return out, srv, g
}

func TestRequestLine(t *testing.T) {
	up := fake(t, completion)
	dir := t.TempDir()
	out, srv, g := heard(t, solo(dir, up.URL))

	post(t, g, `{"model":"fast","messages":[]}`)

	m := out.find(t, "request")
	want := map[string]any{
		"model": "fast", "sent": "m", "provider": "one", "outcome": "ok",
		"key": srv.pool.keys["one"][0].id, "tries": float64(1),
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("request line %s = %v, want %v", k, m[k], v)
		}
	}
	if _, ok := m["ms"]; !ok {
		t.Error("request line carries no latency")
	}
}

func TestFailoverLine(t *testing.T) {
	dead, live := fake(t, boom(500)), fake(t, completion)
	dir := t.TempDir()
	c := solo(dir, dead.URL)
	c.Providers["two"] = &Provider{URL: live.URL + "/v1", Keys: []string{"k-two"}}
	c.Aliases["fast"] = []string{"one/m", "two/m"}
	out, srv, g := heard(t, c)

	post(t, g, `{"model":"fast","messages":[]}`)

	m := out.find(t, "attempt failed")
	if m["provider"] != "one" || m["code"] != float64(500) {
		t.Errorf("failover line = %v, want the provider and code that failed", m)
	}
	if m["benched"] != "1m0s" || m["dead"] != false {
		t.Errorf("bench = %v/%v, want the ladder rung the failure cost, not a dead key", m["benched"], m["dead"])
	}
	if m["key"] != srv.pool.keys["one"][0].id {
		t.Errorf("key = %v, want the fingerprint of the key that failed", m["key"])
	}
	if got := out.find(t, "request")["outcome"]; got != "ok" {
		t.Errorf("outcome = %v, want ok: the failover succeeded", got)
	}
}

func TestExhaustedLine(t *testing.T) {
	dead := fake(t, boom(429))
	dir := t.TempDir()
	out, _, g := heard(t, solo(dir, dead.URL))

	post(t, g, `{"model":"fast","messages":[]}`)

	m := out.find(t, "request")
	if m["outcome"] != "exhausted" {
		t.Errorf("outcome = %v, want exhausted", m["outcome"])
	}
	if m["recovers_in"] != "1m0s" {
		t.Errorf("recovers_in = %v, want when the alias comes back", m["recovers_in"])
	}
	if n := strings.Count(out.String(), `"msg":"request"`); n != 1 {
		t.Errorf("%d request lines, want 1", n)
	}
	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "omny_exhausted") {
		t.Errorf("503 body = %s, want type omny_exhausted", body)
	}
}

func TestLogHidesKeyMaterial(t *testing.T) {
	const secret = "gsk_supersecret_key_material"
	leaky := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":{"message":"invalid api key `+secret+`"}}`)
	})
	dir := t.TempDir()
	c := solo(dir, leaky.URL)
	c.Providers["one"].Keys = []string{secret}
	out, srv, g := heard(t, c)

	post(t, g, `{"model":"fast","messages":[]}`)

	if strings.Contains(out.String(), secret) {
		t.Fatal("the log carries key material")
	}
	if !strings.Contains(out.String(), srv.pool.keys["one"][0].id) {
		t.Error("the log names no key at all: the fingerprint has to survive the scrub")
	}
}

func port(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func boot(t *testing.T, dir string, c *Config) (string, string, chan error) {
	t.Helper()
	c.Listen = port(t)
	path := file(t, dir, c)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() { done <- Run(ctx, path); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })

	url := "http://" + c.Listen
	until(t, "the gateway to listen", func() bool {
		res, err := http.Get(url + "/healthz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return true
	})
	return url, path, done
}

func TestHupReloads(t *testing.T) {
	up := fake(t, completion)
	dir := t.TempDir()
	c := solo(dir, up.URL)
	url, _, _ := boot(t, dir, c)

	if strings.Contains(models(t, url), `"slow"`) {
		t.Fatal("the alias exists before the reload")
	}

	c.Aliases["slow"] = []string{"one/m"}
	file(t, dir, c)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	until(t, "SIGHUP to apply the new alias", func() bool {
		return strings.Contains(models(t, url), `"slow"`)
	})
}

func models(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDrainFinishesInflight(t *testing.T) {
	release := make(chan struct{})
	up := fake(t, held(release, token("a"), token("b")))
	dir := t.TempDir()
	c := solo(dir, up.URL)
	c.Listen = port(t)
	path := file(t, dir, c)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path) }()
	url := "http://" + c.Listen
	until(t, "the gateway to listen", func() bool {
		res, err := http.Get(url + "/healthz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return true
	})

	body := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions",
			strings.NewReader(`{"model":"fast","stream":true,"messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			body <- "request failed: " + err.Error()
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		body <- string(b)
	}()
	until(t, "the stream to reach the provider", func() bool { return len(up.calls()) == 1 })

	cancel()
	select {
	case err := <-done:
		t.Fatalf("run returned while a stream was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	out := <-body
	for _, want := range []string{`"a"`, `"b"`, "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("shutdown truncated the stream, lost %s:\n%s", want, out)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("run = %v, want a clean exit", err)
	}
}

func TestRecoveryLine(t *testing.T) {
	up := fake(t, completion)
	dir := t.TempDir()
	out, srv, g := heard(t, solo(dir, up.URL))

	k := srv.pool.keys["one"][0]
	srv.pool.fail(k, 429, time.Hour)
	k.until = time.Time{}.Add(time.Nanosecond)

	post(t, g, `{"model":"fast","messages":[]}`)

	if got := out.find(t, "key recovered")["key"]; got != k.id {
		t.Errorf("recovery line key = %v, want %s", got, k.id)
	}
	post(t, g, `{"model":"fast","messages":[]}`)
	if n := strings.Count(out.String(), "key recovered"); n != 1 {
		t.Errorf("%d recovery lines, want 1: a healthy key does not recover on every request", n)
	}
}

func TestInflightKeepsItsProvider(t *testing.T) {
	release := make(chan struct{})
	var n atomic.Int32
	here := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			<-release
			boom(500)(w, r)
			return
		}
		completion(w, r)
	})
	moved := fake(t, completion)

	dir := t.TempDir()
	c := base(dir)
	c.Default = "one"
	c.Providers = map[string]*Provider{"one": {URL: here.URL + "/v1", Keys: []string{"k1", "k2"}}}
	c.Aliases = map[string][]string{"fast": {"one/m"}, "spare": {"one/m"}}
	c.Fallback = []string{"spare"}
	srv, g := stand(t, c)

	code := make(chan int, 1)
	go func() {
		res, _ := post(t, g, `{"model":"fast","messages":[]}`)
		code <- res.StatusCode
	}()
	until(t, "the first hop to reach its provider", func() bool { return len(here.calls()) == 1 })

	moving := base(dir)
	moving.Default = "one"
	moving.Providers = map[string]*Provider{"one": {
		URL:     moved.URL + "/v1",
		Headers: map[string]string{"X-Omny-Moved": "yes"},
		Keys:    []string{"k1", "k2"},
	}}
	moving.Aliases = c.Aliases
	moving.Fallback = c.Fallback
	if err := srv.reload(file(t, dir, moving)); err != nil {
		t.Fatal(err)
	}
	close(release)

	if got := <-code; got != http.StatusOK {
		t.Fatalf("status = %d, want the second hop to serve", got)
	}
	if len(moved.calls()) != 0 {
		t.Error("a reload re-pointed a provider a request was already walking")
	}
	calls := here.calls()
	if len(calls) != 2 {
		t.Fatalf("original provider saw %d calls, want 2", len(calls))
	}
	if got := calls[1].head.Get("X-Omny-Moved"); got != "" {
		t.Errorf("second hop carried the reloaded provider's headers (%q)", got)
	}
	post(t, g, `{"model":"fast","messages":[]}`)
	if len(moved.calls()) != 1 {
		t.Errorf("the next request saw %d calls at the new url, want 1", len(moved.calls()))
	}
}

func TestDeadKeyLine(t *testing.T) {
	up := fake(t, boom(401))
	dir := t.TempDir()
	out, _, g := heard(t, solo(dir, up.URL))

	post(t, g, `{"model":"fast","messages":[]}`)

	m := out.find(t, "attempt failed")
	if m["dead"] != true {
		t.Errorf("dead = %v, want true: 401 is credentials, and no cooldown fixes it", m["dead"])
	}
	if m["benched"] != "0s" {
		t.Errorf("benched = %v, want no cooldown on a dead key", m["benched"])
	}
}

func TestUnauthorizedLine(t *testing.T) {
	up := fake(t, completion)
	dir := t.TempDir()
	out, _, g := heard(t, solo(dir, up.URL))

	hit(t, g, http.MethodPost, "/v1/chat/completions", "Bearer wrong", `{"model":"fast","messages":[]}`)

	if got := out.find(t, "unauthorized")["path"]; got != "/v1/chat/completions" {
		t.Errorf("unauthorized line path = %v", got)
	}
	if strings.Contains(out.String(), "wrong") {
		t.Error("the log echoes the token that was offered")
	}
}

func TestScrubOutlivesTheConfig(t *testing.T) {
	const secret = "gsk_removed_but_still_in_flight"
	up := fake(t, completion)
	dir := t.TempDir()
	c := solo(dir, up.URL)
	c.Providers["one"].Keys = []string{secret, "k-keeper"}
	srv, _ := stand(t, c)

	n := solo(dir, up.URL)
	n.Providers["one"].Keys = []string{"k-keeper"}
	if err := srv.reload(file(t, dir, n)); err != nil {
		t.Fatal(err)
	}
	if got := srv.pool.scrub("provider says " + secret); strings.Contains(got, secret) {
		t.Errorf("scrub(%q) = %q, want the removed key still masked", secret, got)
	}
}
