package omny

import (
	"net/http"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestMeasureSmooths(t *testing.T) {
	cases := []struct {
		name    string
		samples []time.Duration
		want    time.Duration
	}{
		{"unmeasured", nil, 0},
		{"first sample is taken whole", []time.Duration{300 * time.Millisecond}, 300 * time.Millisecond},
		{"a repeat does not drift", []time.Duration{300 * time.Millisecond, 300 * time.Millisecond}, 300 * time.Millisecond},
		{"one outlier moves it without defining it",
			[]time.Duration{300 * time.Millisecond, 1000 * time.Millisecond}, 510 * time.Millisecond},
		{"a degraded provider is reflected quickly",
			[]time.Duration{100 * time.Millisecond, time.Second, time.Second, time.Second, time.Second},
			78391 * time.Millisecond / 100},
		{"a clock that did not advance is not a measurement",
			[]time.Duration{300 * time.Millisecond, 0, -5}, 300 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var k Key
			for _, d := range c.samples {
				k.measure(d)
			}
			if k.ttft != c.want {
				t.Errorf("ttft=%v, want %v", k.ttft, c.want)
			}
		})
	}
}

func TestFailureIsNotAMeasurement(t *testing.T) {
	p, _ := stub(t, 0, "k")
	k := p.pick("p")
	p.fail(k, 500, 0)
	if k.ttft != 0 {
		t.Errorf("a failure measured %v; only a success is a sample", k.ttft)
	}
}

func TestSuccessMeasures(t *testing.T) {
	p, _ := stub(t, 0, "k")
	k := p.pick("p")
	if k.ttft != 0 {
		t.Fatalf("a fresh key starts measured at %v", k.ttft)
	}
	p.ok(k, 250*time.Millisecond)
	if k.ttft != 250*time.Millisecond {
		t.Errorf("ttft=%v, want 250ms", k.ttft)
	}
}

func TestLatencyOutlivesTheDay(t *testing.T) {
	p, cl := stub(t, 100, "k")
	k := p.pick("p")
	p.ok(k, 400*time.Millisecond)
	if err := p.save(); err != nil {
		t.Fatal(err)
	}

	cl.tick(25 * time.Hour)
	q, _ := stub(t, 100, "k")
	q.path, q.now = p.path, cl.now
	q.day = cl.t.UTC().YearDay()
	if err := q.restore(); err != nil {
		t.Fatal(err)
	}

	r := q.pick("p")
	if r.ttft != 400*time.Millisecond {
		t.Errorf("ttft=%v after a restart, want 400ms — latency is not day-scoped", r.ttft)
	}
	if r.reqs != 0 {
		t.Errorf("reqs=%d after a day boundary, want 0 — yesterday's quota is not today's", r.reqs)
	}
}

func fleet(t *testing.T, names ...string) (*Config, *Pool) {
	t.Helper()
	c := &Config{
		State:     filepath.Join(t.TempDir(), "omny.state.json"),
		Providers: make(map[string]*Provider, len(names)),
		Aliases:   map[string][]string{"a": nil},
	}
	for _, n := range names {
		c.Providers[n] = &Provider{URL: "http://" + n, Keys: []string{"k-" + n}}
		c.Aliases["a"] = append(c.Aliases["a"], n+"/m")
	}
	return c, pool(c)
}

func provs(ts []target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.prov
	}
	return out
}

func TestOrderFollowsMeasurement(t *testing.T) {
	c, p := fleet(t, "slow", "quick")
	p.ok(p.pick("slow"), 4*time.Second)
	p.ok(p.pick("quick"), 200*time.Millisecond)

	ts := c.route("a")
	p.order(ts)
	if got := provs(ts); !slices.Equal(got, []string{"quick", "slow"}) {
		t.Errorf("order %v, want [quick slow] — measurement did not beat config order", got)
	}
}

func TestOrderHoldsInsideTheDeadband(t *testing.T) {
	c, p := fleet(t, "first", "second")
	p.ok(p.pick("first"), 400*time.Millisecond)
	p.ok(p.pick("second"), 300*time.Millisecond)

	ts := c.route("a")
	p.order(ts)
	if got := provs(ts); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("order %v, want [first second] — 100ms is noise, not a finding", got)
	}
}

func TestUnmeasuredKeepsItsPlace(t *testing.T) {
	c, p := fleet(t, "slow", "fresh", "quick")
	p.ok(p.pick("slow"), 4*time.Second)
	p.ok(p.pick("quick"), 200*time.Millisecond)

	ts := c.route("a")
	p.order(ts)
	if got := provs(ts); !slices.Equal(got, []string{"quick", "fresh", "slow"}) {
		t.Errorf("order %v, want [quick fresh slow] — the unmeasured target moved", got)
	}
}

func TestOrderIgnoresBenchedKeys(t *testing.T) {
	c, p := fleet(t, "slow", "quick")
	p.ok(p.pick("slow"), 4*time.Second)
	k := p.pick("quick")
	p.ok(k, 200*time.Millisecond)
	p.fail(k, 500, 0)

	ts := c.route("a")
	p.order(ts)
	if got := provs(ts); !slices.Equal(got, []string{"slow", "quick"}) {
		t.Errorf("order %v, want [slow quick] — a benched key was ranked as reachable", got)
	}
}

func TestOrderLeavesAPinAlone(t *testing.T) {
	c, p := fleet(t, "slow", "quick")
	p.ok(p.pick("quick"), 200*time.Millisecond)

	ts := c.route("slow/m")
	p.order(ts)
	if got := provs(ts); !slices.Equal(got, []string{"slow"}) {
		t.Errorf("order %v, want [slow] — a pin was rerouted", got)
	}
}

func TestTighten(t *testing.T) {
	const floor = 2 * time.Second
	cases := []struct {
		name       string
		ttft, want time.Duration
		got        time.Duration
	}{
		{"unmeasured keeps the whole budget", 0, 15 * time.Second, 15 * time.Second},
		{"three times the average", 2 * time.Second, 15 * time.Second, 6 * time.Second},
		{"the floor stops it going silly", 10 * time.Millisecond, 15 * time.Second, floor},
		{"a slow provider is not given longer", 30 * time.Second, 15 * time.Second, 15 * time.Second},
		{"a declared ttft is still the ceiling", 10 * time.Second, 60 * time.Second, 30 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := stub(t, 0, "k")
			k := p.pick("p")
			k.ttft = c.ttft
			if got := p.tighten(k, c.want, floor); got != c.got {
				t.Errorf("tighten(%v, %v) = %v, want %v", c.ttft, c.want, got, c.got)
			}
		})
	}
}

func TestMeasuredBudgetFailsOverFast(t *testing.T) {
	var hang atomic.Bool
	quick := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			<-r.Context().Done()
			return
		}
		sse(token("hi"))(w, r)
	})
	backup := fake(t, sse(token("yo")))
	srv, g := stand(t, &Config{
		Default: "quick",
		Aliases: map[string][]string{"a": {"quick/m", "backup/m"}},
		Providers: map[string]*Provider{
			"quick":  {URL: quick.URL + "/v1", Keys: []string{"k1"}},
			"backup": {URL: backup.URL + "/v1", Keys: []string{"k2"}},
		},
	})
	srv.ttft, srv.idle, srv.floor = time.Hour, time.Hour, 50*time.Millisecond

	if res, b := post(t, g, `{"model":"a","stream":true,"messages":[]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("the measuring request got %d: %s", res.StatusCode, b)
	}
	if n := len(backup.calls()); n != 0 {
		t.Fatalf("the backup served the first request; there was nothing to measure")
	}

	hang.Store(true)
	start := time.Now()
	res, b := post(t, g, `{"model":"a","stream":true,"messages":[]}`)
	took := time.Since(start)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", res.StatusCode, b)
	}
	if n := len(backup.calls()); n != 1 {
		t.Errorf("the backup was called %d times, want 1", n)
	}
	if took > 5*time.Second {
		t.Errorf("failover took %v; the measured budget was ignored", took)
	}
}

func TestMeasuredBudgetSparesNonStreaming(t *testing.T) {
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if q, _ := peek(mustRead(t, r)); q.Stream {
			sse(token("hi"))(w, r)
			return
		}
		time.Sleep(300 * time.Millisecond)
		completion(w, r)
	})
	backup := fake(t, completion)
	srv, g := stand(t, &Config{
		Default: "slowish",
		Aliases: map[string][]string{"a": {"slowish/m", "backup/m"}},
		Providers: map[string]*Provider{
			"slowish": {URL: up.URL + "/v1", Keys: []string{"k1"}},
			"backup":  {URL: backup.URL + "/v1", Keys: []string{"k2"}},
		},
	})
	srv.ttft, srv.idle, srv.floor = time.Hour, 30*time.Second, 10*time.Millisecond

	post(t, g, `{"model":"a","stream":true,"messages":[]}`)
	if res, b := post(t, g, `{"model":"a","messages":[]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	if n := len(backup.calls()); n != 0 {
		t.Errorf("the backup served %d requests; a stream's measurement cut short a completion", n)
	}
}

func TestTier(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want int
	}{
		{0, 0}, {10 * time.Millisecond, 0}, {250 * time.Millisecond, 0},
		{251 * time.Millisecond, 1}, {499 * time.Millisecond, 1},
		{501 * time.Millisecond, 2}, {999 * time.Millisecond, 2},
		{time.Second, 3}, {4 * time.Second, 5},
	}
	for _, c := range cases {
		if got := tier(c.d); got != c.want {
			t.Errorf("tier(%v) = %d, want %d", c.d, got, c.want)
		}
	}
}
