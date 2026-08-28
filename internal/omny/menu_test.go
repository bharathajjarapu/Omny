package omny

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInertProviderIsSkipped(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default: "live",
		Aliases: map[string][]string{"fast": {"empty/m", "live/m"}},
		Providers: map[string]*Provider{
			"empty": {URL: "http://127.0.0.1:1/v1"},
			"live":  {URL: up.URL + "/v1", Keys: []string{"k"}},
		},
	})
	res, body := post(t, g, `{"model":"fast","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	if n := len(up.calls()); n != 1 {
		t.Errorf("the live provider was called %d times, want 1", n)
	}
}

func TestInertOnlyAliasExhausts(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default: "live",
		Aliases: map[string][]string{"cold": {"empty/m"}},
		Providers: map[string]*Provider{
			"empty": {URL: "http://127.0.0.1:1/v1"},
			"live":  {URL: up.URL + "/v1", Keys: []string{"k"}},
		},
	})
	res, body := post(t, g, `{"model":"cold","messages":[]}`)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", res.StatusCode, body)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("an alias naming no armed provider still sent %d requests", n)
	}
}

func TestInertProviderHasNoCard(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default: "live",
		Providers: map[string]*Provider{
			"empty": {URL: "http://127.0.0.1:1/v1"},
			"live":  {URL: up.URL + "/v1", Keys: []string{"k"}},
		},
	})
	got := byProv(cards(t, g, ""))
	if _, ok := got["empty"]; ok {
		t.Error("an inert provider contributed a card, but it has no key to report on")
	}
	if len(got) != 1 {
		t.Errorf("got %d cards, want 1", len(got))
	}
}

func TestKeylessSendsNoAuthorization(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "ovh",
		Providers: map[string]*Provider{"ovh": {URL: up.URL + "/v1", Keyless: true}},
	})
	res, body := post(t, g, `{"model":"m","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
	}
	if auth := up.calls()[0].auth; auth != "" {
		t.Errorf("sent %q, want no authorization header at all", auth)
	}
}

func TestKeylessProvidersDoNotCollide(t *testing.T) {
	a, b := fake(t, completion), fake(t, completion)
	g := gateway(t, &Config{
		Default: "ovh",
		Providers: map[string]*Provider{
			"ovh":  {URL: a.URL + "/v1", Keyless: true},
			"kilo": {URL: b.URL + "/v1", Keyless: true},
		},
	})
	got := cards(t, g, "")
	if len(got) != 2 {
		t.Fatalf("got %d cards, want one per keyless provider", len(got))
	}
	if got[0].Key == got[1].Key {
		t.Errorf("both keyless providers report id %q, so they share a state-file line", got[0].Key)
	}
}

func TestKeylessBenchesLikeAnyOtherKey(t *testing.T) {
	up := fake(t, boom(429))
	g := gateway(t, &Config{
		Default:   "ovh",
		Providers: map[string]*Provider{"ovh": {URL: up.URL + "/v1", RPD: 50, Keyless: true}},
	})
	post(t, g, `{"model":"m","messages":[]}`)

	got := byProv(cards(t, g, ""))["ovh"]
	if got.Till == "" {
		t.Error("a keyless entry did not bench")
	}
	if got.Errs != 1 {
		t.Errorf("errs = %d, want 1", got.Errs)
	}
}

func TestProviderKeepsItsOwnFirstTokenBudget(t *testing.T) {
	up := fake(t, slow(200*time.Millisecond))
	srv, g := stand(t, &Config{
		Default:   "nvidia",
		Providers: map[string]*Provider{"nvidia": {URL: up.URL + "/v1", TTFT: 5 * time.Second, Keys: []string{"k"}}},
	})
	srv.ttft, srv.idle = 20*time.Millisecond, 20*time.Millisecond

	res, body := post(t, g, `{"model":"m","messages":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d — the provider's own budget was not honoured: %s", res.StatusCode, body)
	}
}

func TestMinuteCapSkipsRatherThanBenches(t *testing.T) {
	p, cl := rig(t, &Provider{URL: "http://x", RPM: 2, Keys: []string{"k"}})

	for i := range 2 {
		k := p.pick("p")
		if k == nil {
			t.Fatalf("request %d: no key, but the minute allows two", i)
		}
		p.ok(k, 0)
	}
	if p.pick("p") != nil {
		t.Fatal("a key at its per-minute limit was picked")
	}
	if k := p.keys["p"][0]; !k.until.IsZero() {
		t.Error("hitting the minute cap benched the key, but it was never at fault")
	}
	if d := p.soonest([]target{{prov: "p"}}); d <= 0 || d > time.Minute {
		t.Errorf("soonest = %v, want the rest of the minute", d)
	}
	cl.tick(time.Minute)
	if p.pick("p") == nil {
		t.Error("the minute rolled over and the key is still skipped")
	}
}

func TestUnsetMinuteCapNeverSkips(t *testing.T) {
	p, _ := rig(t, &Provider{URL: "http://x", Keys: []string{"k"}})
	for i := range 5 {
		k := p.pick("p")
		if k == nil {
			t.Fatalf("request %d: an unset rpm behaved like a limit of zero", i)
		}
		p.ok(k, 0)
	}
}

func TestMinuteCounterIsNotPersisted(t *testing.T) {
	p, _ := rig(t, &Provider{URL: "http://x", RPM: 2, Keys: []string{"k"}})
	p.ok(p.pick("p"), 0)
	if err := p.save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "mins") || strings.Contains(string(b), "rpm") {
		t.Errorf("the state file carries the minute window: %s", b)
	}
}

func TestStatusShowsBothWindows(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", RPD: 50, RPM: 3, Keys: []string{"k"}}},
	})
	post(t, g, `{"model":"m","messages":[]}`)

	got := byProv(cards(t, g, ""))["groq"]
	if got.Left == nil || *got.Left != 49 {
		t.Errorf("left = %v, want 49", got.Left)
	}
	if got.Spare == nil || *got.Spare != 2 {
		t.Errorf("spare = %v, want 2", got.Spare)
	}
}

func TestStatusLeavesUnknownLimitsNull(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	res := ask(t, g, http.MethodGet, "/status", "")
	body := string(read(t, res))
	if !strings.Contains(body, `"left":null`) || !strings.Contains(body, `"spare":null`) {
		t.Errorf("an unknown limit is not reported as unknown: %s", body)
	}
}

func TestKeylessLeavesTheScrubberIntact(t *testing.T) {
	p := pool(&Config{
		State: filepath.Join(t.TempDir(), "omny.state.json"),
		Providers: map[string]*Provider{
			"ovh":  {URL: "http://x", Keyless: true},
			"groq": {URL: "http://y", Keys: []string{"sk-live-secret"}},
		},
	})
	got := p.scrub("groq said sk-live-secret is bad")
	if strings.Contains(got, "sk-live-secret") {
		t.Fatalf("scrub leaked the key: %q", got)
	}
	if want := "groq said " + Fingerprint("sk-live-secret") + " is bad"; got != want {
		t.Errorf("scrub returned %q, want %q", got, want)
	}
}

const catalogue = "../../omny.example.yaml"

func TestCatalogueLoads(t *testing.T) {
	body, err := os.ReadFile(catalogue)
	if err != nil {
		t.Fatal(err)
	}
	path := write(t, strings.Replace(string(body), "token:", "token: "+tok, 1), 0o600)

	start := time.Now()
	c, err := Load(path)
	if err != nil {
		t.Fatalf("the catalogue does not load: %v", err)
	}
	p := pool(c)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("loading and adopting the catalogue took %v", d)
	}
	if len(c.Providers) < 30 {
		t.Errorf("the catalogue lists %d providers, want at least 30", len(c.Providers))
	}
	for n, at := range c.Providers {
		if len(at.Keys) > 0 {
			t.Errorf("provider %s ships with a key", n)
		}
	}
	on := p.report()
	if len(on) != 2 {
		t.Errorf("%d entries are armed out of the box, want the two keyless ones", len(on))
	}
	for _, card := range on {
		if !c.Providers[card.Prov].Keyless {
			t.Errorf("provider %s is armed but not keyless", card.Prov)
		}
	}
}

func TestCatalogueAliasesResolve(t *testing.T) {
	body, err := os.ReadFile(catalogue)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(write(t, strings.Replace(string(body), "token:", "token: "+tok, 1), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	for name := range c.Aliases {
		for _, tg := range c.route(name) {
			if tg.at == nil {
				t.Errorf("alias %s names provider %q, which the catalogue does not list", name, tg.prov)
			}
		}
	}
}

func TestDuplicateKeyIsRefused(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"within one provider", "token: t\ndefault: a\nproviders:\n  a:\n    url: https://x/v1\n    keys: [k, k]\n"},
		{"across two", "token: t\ndefault: a\nproviders:\n  a:\n    url: https://x/v1\n    keys: [k]\n  b:\n    url: https://y/v1\n    keys: [k]\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.body, 0o600))
			if err == nil || !strings.Contains(err.Error(), "one key is one pool slot") {
				t.Fatalf("load returned %v, want a duplicate-key refusal", err)
			}
		})
	}
}

func TestCountersRollWhereTheyAreWritten(t *testing.T) {
	t.Run("a success", func(t *testing.T) {
		p, cl := rig(t, &Provider{URL: "http://x", RPD: 10, RPM: 10, Keys: []string{"k"}})
		p.ok(p.pick("p"), 0)
		k := p.pick("p")
		cl.tick(25 * time.Hour)
		p.ok(k, 0)
		if k.reqs != 1 || k.mins != 1 {
			t.Errorf("reqs=%d mins=%d, want 1 each — yesterday's counts carried over", k.reqs, k.mins)
		}
	})
	t.Run("a failure", func(t *testing.T) {
		p, cl := rig(t, &Provider{URL: "http://x", Keys: []string{"k"}})
		p.fail(p.pick("p"), 500, 0)
		cl.tick(2 * time.Minute)
		k := p.pick("p")
		cl.tick(25 * time.Hour)
		p.fail(k, 500, 0)
		if k.errs != 1 {
			t.Errorf("errs=%d, want 1 — yesterday's errors carried over", k.errs)
		}
	})
}
