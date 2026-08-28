package omny

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shop(t *testing.T, models, chat int) *upstream {
	t.Helper()
	return fake(t, func(w http.ResponseWriter, r *http.Request) {
		code, list := chat, strings.HasSuffix(r.URL.Path, "/models")
		if list {
			code = models
		}
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		if list {
			io.WriteString(w, `{"object":"list","data":[]}`)
			return
		}
		completion(w, r)
	})
}

func cards(t *testing.T, g *httptest.Server, query string) []card {
	t.Helper()
	res := ask(t, g, http.MethodGet, "/status"+query, "")
	body := read(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	var got []card
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return got
}

func shown(t *testing.T, cs []card) string {
	t.Helper()
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func byProv(cs []card) map[string]card {
	m := make(map[string]card, len(cs))
	for _, c := range cs {
		m[c.Prov] = c
	}
	return m
}

func TestProbeShallow(t *testing.T) {
	live, revoked := shop(t, 200, 200), shop(t, 401, 401)
	g := gateway(t, &Config{
		Default: "live",
		Providers: map[string]*Provider{
			"live":    {URL: live.URL + "/v1", Keys: []string{"good"}},
			"revoked": {URL: revoked.URL + "/v1", Keys: []string{"bad"}},
		},
	})
	got := byProv(cards(t, g, "?probe=models"))

	if got["live"].Probe != "ok" || got["live"].Code != 200 {
		t.Errorf("live probed %q/%d, want ok/200", got["live"].Probe, got["live"].Code)
	}
	if got["revoked"].Probe != "rejected" || got["revoked"].Code != 401 {
		t.Errorf("revoked probed %q/%d, want rejected/401", got["revoked"].Probe, got["revoked"].Code)
	}
	for _, u := range []*upstream{live, revoked} {
		if n := len(u.calls()); n != 1 {
			t.Errorf("provider called %d times, want 1", n)
		} else if p := u.calls()[0].path; p != "/v1/models" {
			t.Errorf("shallow probe asked %s, want /v1/models — a chat spends quota", p)
		}
	}
}

func TestProbeDeep(t *testing.T) {
	up := shop(t, 200, 402)
	g := gateway(t, &Config{
		Default:   "cerebras",
		Aliases:   map[string][]string{"fast": {"cerebras/gpt-oss-120b"}},
		Providers: map[string]*Provider{"cerebras": {URL: up.URL + "/v1", Keys: []string{"csk"}}},
	})
	if got := byProv(cards(t, g, "?probe=models"))["cerebras"]; got.Probe != "ok" {
		t.Errorf("shallow probed %q, want ok", got.Probe)
	}
	got := byProv(cards(t, g, "?probe=chat"))["cerebras"]
	if got.Probe != "rejected" || got.Code != 402 {
		t.Errorf("deep probed %q/%d, want rejected/402", got.Probe, got.Code)
	}

	last := up.calls()[len(up.calls())-1]
	if last.path != "/v1/chat/completions" {
		t.Errorf("deep probe asked %s, want the chat endpoint", last.path)
	}
	if last.model != "gpt-oss-120b" {
		t.Errorf("deep probe asked for %q, want the model an alias routes there", last.model)
	}
	if !strings.Contains(string(last.body), `"max_tokens":1`) {
		t.Errorf("deep probe body %s does not cap the spend at one token", last.body)
	}
}

func TestProbeDeepWithoutAnAlias(t *testing.T) {
	up := shop(t, 200, 200)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	if got := byProv(cards(t, g, "?probe=chat"))["groq"]; got.Probe != "unprobeable" {
		t.Errorf("probed %q, want unprobeable", got.Probe)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("guessed a model and called the provider %d times", n)
	}
}

func TestProbeNeverTouchesThePool(t *testing.T) {
	up := shop(t, 401, 401)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", RPD: 5, RPM: 3, Keys: []string{"k"}}},
	})
	before := shown(t, cards(t, g, ""))
	if got := byProv(cards(t, g, "?probe=models"))["groq"]; got.Probe != "rejected" {
		t.Fatalf("probed %q, want rejected", got.Probe)
	}
	if after := shown(t, cards(t, g, "")); after != before {
		t.Errorf("the probe changed the pool:\n before %s\n after  %s", before, after)
	}
}

func TestStatusStaysPassive(t *testing.T) {
	up := shop(t, 200, 200)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	got := byProv(cards(t, g, ""))["groq"]
	if got.Probe != "" || got.Code != 0 {
		t.Errorf("a plain /status reported a probe: %+v", got)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("a plain /status made %d upstream calls, want 0", n)
	}
}

func TestProbeUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()

	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: url + "/v1", Keys: []string{"k"}}},
	})
	if got := byProv(cards(t, g, "?probe=models"))["groq"]; got.Probe != "unreachable" || got.Code != 0 {
		t.Errorf("probed %q/%d, want unreachable/0", got.Probe, got.Code)
	}
}

func TestProbeRejectsAnUnknownDepth(t *testing.T) {
	up := shop(t, 200, 200)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	res := ask(t, g, http.MethodGet, "/status?probe=deep", "")
	body := read(t, res)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "spends") {
		t.Errorf("the refusal does not say which depth costs quota: %s", body)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("an unknown depth still probed %d times", n)
	}
}

func TestProbeHidesKeyMaterial(t *testing.T) {
	up := shop(t, 401, 401)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"sk-live-secret"}}},
	})
	res := ask(t, g, http.MethodGet, "/status?probe=models", "")
	if body := read(t, res); strings.Contains(string(body), "sk-live-secret") {
		t.Fatalf("the probe leaked the key: %s", body)
	}
	if auth := up.calls()[0].auth; auth != "Bearer sk-live-secret" {
		t.Errorf("the probe sent %q, want the real key — it must ask as the relay does", auth)
	}
}

func TestProbeFansOut(t *testing.T) {
	var live, peak atomic.Int64
	slowly := func(w http.ResponseWriter, _ *http.Request) {
		n := live.Add(1)
		for {
			was := peak.Load()
			if n <= was || peak.CompareAndSwap(was, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		live.Add(-1)
		io.WriteString(w, `{"data":[]}`)
	}
	c := &Config{Default: "a", Providers: map[string]*Provider{}}
	for i := range 30 {
		up := fake(t, slowly)
		n := string(rune('a' + i))
		c.Providers[n] = &Provider{URL: up.URL + "/v1", Keys: []string{"k-" + n}}
	}
	g := gateway(t, c)

	start := time.Now()
	if n := len(cards(t, g, "?probe=models")); n != 30 {
		t.Fatalf("probed %d keys, want 30", n)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("probing 30 providers took %v, want the fan-out to overlap them", d)
	}
	if got := peak.Load(); got > fanout {
		t.Errorf("%d probes were in flight at once, want at most %d", got, fanout)
	} else if got < 2 {
		t.Errorf("peak concurrency was %d, so nothing overlapped", got)
	}
}

func TestProbeKeepsTheProvidersBudget(t *testing.T) {
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		if strings.HasSuffix(r.URL.Path, "/models") {
			io.WriteString(w, `{"data":[]}`)
			return
		}
		completion(w, r)
	})
	srv, g := stand(t, &Config{
		Default:   "gemini",
		Aliases:   map[string][]string{"fast": {"gemini/gemini-3.6-flash"}},
		Providers: map[string]*Provider{"gemini": {URL: up.URL + "/v1", TTFT: 5 * time.Second, Keys: []string{"k"}}},
	})
	srv.patience = 20 * time.Millisecond

	if got := byProv(cards(t, g, "?probe=chat"))["gemini"]; got.Probe != "ok" {
		t.Errorf("deep probed %q, want ok — the provider declares a longer budget", got.Probe)
	}
	if got := byProv(cards(t, g, "?probe=models"))["gemini"]; got.Probe != "timeout" {
		t.Errorf("shallow probed %q, want timeout — a catalogue GET keeps the short budget", got.Probe)
	}
}

func TestProbeGivesUp(t *testing.T) {
	up := fake(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		io.WriteString(w, `{"data":[]}`)
	})
	srv, g := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	srv.patience = 20 * time.Millisecond

	start := time.Now()
	if got := byProv(cards(t, g, "?probe=models"))["groq"]; got.Probe != "timeout" {
		t.Errorf("probed %q, want timeout", got.Probe)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the probe waited %v on a hung provider", d)
	}
}

func TestProbeRedirectIsNotOk(t *testing.T) {
	up := shop(t, http.StatusFound, 200)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	got := byProv(cards(t, g, "?probe=models"))["groq"]
	if got.Probe != "rejected" || got.Code != http.StatusFound {
		t.Errorf("probed %q/%d, want rejected/302", got.Probe, got.Code)
	}
}
