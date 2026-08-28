package omny

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time       { return c.t }
func (c *clock) tick(d time.Duration) { c.t = c.t.Add(d) }

func stub(t *testing.T, rpd int, keys ...string) (*Pool, *clock) {
	t.Helper()
	return rig(t, &Provider{URL: "http://x", RPD: rpd, Keys: keys})
}

func rig(t *testing.T, at *Provider) (*Pool, *clock) {
	t.Helper()
	c := &Config{
		State:     filepath.Join(t.TempDir(), "omny.state.json"),
		Providers: map[string]*Provider{"p": at},
	}
	cl := &clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	p := pool(c)
	p.now, p.day, p.min = cl.now, cl.t.UTC().YearDay(), cl.t.Unix()/60
	return p, cl
}

func TestLadder(t *testing.T) {
	p, cl := stub(t, 0, "k")
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 24 * time.Hour, 24 * time.Hour}

	for i, d := range want {
		k := p.pick("p")
		if k == nil {
			t.Fatalf("rung %d: no key, but the previous cooldown should have expired", i)
		}
		p.fail(k, 500, 0)
		if got := k.until.Sub(cl.t); got != d {
			t.Errorf("rung %d: benched for %v, want %v", i, got, d)
		}
		if p.pick("p") != nil {
			t.Errorf("rung %d: a benched key was picked", i)
		}
		cl.tick(d)
	}

	k := p.pick("p")
	p.ok(k, 0)
	p.fail(k, 500, 0)
	if got := k.until.Sub(cl.t); got != time.Minute {
		t.Errorf("after a success the ladder restarts at %v, want 1m", got)
	}
}

func TestLadderRetryWins(t *testing.T) {
	p, cl := stub(t, 0, "k")

	k := p.pick("p")
	p.fail(k, 429, 3*time.Second)
	if got := k.until.Sub(cl.t); got != 3*time.Second {
		t.Errorf("benched for %v, want the provider's 3s", got)
	}
	cl.tick(3 * time.Second)

	k = p.pick("p")
	if k == nil {
		t.Fatal("the key never came back off the provider's own cooldown")
	}
	p.fail(k, 500, 0)
	if got := k.until.Sub(cl.t); got != time.Minute {
		t.Errorf("the guided failure advanced the ladder to %v, want the first rung", got)
	}
}

func TestFailClassifies(t *testing.T) {
	for _, c := range []struct {
		name    string
		code    int
		dead    bool
		benched bool
	}{
		{"unauthorized", 401, true, false},
		{"forbidden", 403, true, false},
		{"bad request", 400, false, false},
		{"payment required", 402, false, true},
		{"unprocessable", 422, false, false},
		{"not found", 404, false, false},
		{"rate limited", 429, false, true},
		{"server error", 500, false, true},
		{"never reached the provider", 0, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, cl := stub(t, 0, "k")
			k := p.pick("p")
			p.fail(k, c.code, 0)

			if k.dead != c.dead {
				t.Errorf("dead = %v, want %v", k.dead, c.dead)
			}
			if got := k.until.After(cl.t); got != c.benched {
				t.Errorf("benched = %v, want %v", got, c.benched)
			}
			if k.errs != 1 {
				t.Errorf("errs = %d, want 1 — every failure is counted even when it benches nothing", k.errs)
			}
			cl.tick(48 * time.Hour)
			if got := p.pick("p") == nil; got != c.dead {
				t.Errorf("still unusable after 48h = %v, want %v", got, c.dead)
			}
		})
	}
}

func TestPickRotates(t *testing.T) {
	p, _ := stub(t, 0, "a", "b", "c")
	var got []string
	for range 4 {
		got = append(got, p.pick("p").val)
	}
	if want := "a b c a"; strings.Join(got, " ") != want {
		t.Errorf("picked %q, want %q", strings.Join(got, " "), want)
	}

	p.fail(p.keys["p"][1], 500, 0)
	got = nil
	for range 4 {
		got = append(got, p.pick("p").val)
	}
	if want := "c a c a"; strings.Join(got, " ") != want {
		t.Errorf("picked %q, want %q — the benched key must be skipped, not stall the cursor", strings.Join(got, " "), want)
	}
}

func TestQuota(t *testing.T) {
	for _, c := range []struct {
		name string
		rpd  int
		uses int
		free bool
	}{
		{"under the limit", 3, 2, true},
		{"at the limit", 3, 3, false},
		{"limit unknown", 0, 500, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := stub(t, c.rpd, "k")
			for range c.uses {
				k := p.pick("p")
				if k == nil {
					t.Fatalf("key was skipped after %d of %d uses", c.uses, c.rpd)
				}
				p.ok(k, 0)
			}
			if got := p.pick("p") != nil; got != c.free {
				t.Errorf("usable = %v, want %v", got, c.free)
			}
		})
	}
}

func TestQuotaSkipsRatherThanBenches(t *testing.T) {
	p, _ := stub(t, 1, "a", "b")
	p.ok(p.pick("p"), 0)
	k := p.pick("p")
	if k == nil || k.val != "b" {
		t.Fatalf("picked %v, want the key that still has quota", k)
	}
}

func TestQuotaRollsOver(t *testing.T) {
	p, cl := stub(t, 1, "k")
	p.ok(p.pick("p"), 0)
	if p.pick("p") != nil {
		t.Fatal("a spent key was picked before the day turned")
	}
	cl.tick(24 * time.Hour)
	if p.pick("p") == nil {
		t.Fatal("the day turned and the counter did not")
	}
}

func TestQuotaHoldsUnderBurst(t *testing.T) {
	p, _ := stub(t, 3, "k")

	var picked, wg sync.WaitGroup
	var got atomic.Int64
	go_ := make(chan struct{})
	picked.Add(50)
	for range 50 {
		wg.Go(func() {
			k := p.pick("p")
			picked.Done()
			<-go_
			if k == nil {
				return
			}
			got.Add(1)
			p.ok(k, 0)
		})
	}
	picked.Wait()
	close(go_)
	wg.Wait()

	if n := got.Load(); n != 3 {
		t.Errorf("handed out %d keys against rpd 3", n)
	}
	if l := p.keys["p"][0].live; l != 0 {
		t.Errorf("live = %d after every request resolved, want 0", l)
	}
}

func TestStateRoundTrip(t *testing.T) {
	p, cl := stub(t, 10, "k")
	k := p.pick("p")
	p.ok(k, 0)
	p.fail(k, 500, 0)
	if err := p.save(); err != nil {
		t.Fatal(err)
	}

	next, _ := stub(t, 10, "k")
	next.path, next.now = p.path, cl.now
	if err := next.restore(); err != nil {
		t.Fatal(err)
	}
	got := next.keys["p"][0]
	if got.reqs != 1 || got.errs != 1 {
		t.Errorf("restored reqs=%d errs=%d, want 1 and 1", got.reqs, got.errs)
	}
	if !got.until.IsZero() || got.dead {
		t.Error("a cooldown survived the restart; only counters are meant to")
	}
}

func TestStateStaleDay(t *testing.T) {
	p, _ := stub(t, 10, "k")
	old := state{Day: 1, Keys: map[string]tally{Fingerprint("k"): {Reqs: 9, Errs: 3}}}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(p.path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.restore(); err != nil {
		t.Fatal(err)
	}
	if got := p.keys["p"][0].reqs; got != 0 {
		t.Errorf("reqs = %d, want 0 — yesterday's count says nothing about today's quota", got)
	}
}

func TestStateSurvivesReorder(t *testing.T) {
	p, _ := stub(t, 10, "a", "b")
	p.ok(p.pick("p"), 0)
	if err := p.save(); err != nil {
		t.Fatal(err)
	}

	next, _ := stub(t, 10, "b", "a")
	next.path = p.path
	next.day = p.day
	if err := next.restore(); err != nil {
		t.Fatal(err)
	}
	if got := next.keys["p"][1].reqs; got != 1 {
		t.Errorf("key a has reqs=%d after a reorder, want 1", got)
	}
	if got := next.keys["p"][0].reqs; got != 0 {
		t.Errorf("key b inherited reqs=%d from a reorder, want 0", got)
	}
}

func TestStateMissingFile(t *testing.T) {
	p, _ := stub(t, 10, "k")
	if err := p.restore(); err != nil {
		t.Fatalf("first boot with no state file is not an error: %v", err)
	}
}

func TestStateWritesAtomically(t *testing.T) {
	p, _ := stub(t, 10, "k")
	if err := p.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.path + ".tmp"); err == nil {
		t.Error("the temp file outlived the save")
	}
	st, err := os.Stat(p.path)
	if err != nil {
		t.Fatal(err)
	}
	if m := st.Mode().Perm(); m != 0o600 {
		t.Errorf("state file is mode %04o, want 0600", m)
	}
}

func TestSoonest(t *testing.T) {
	ts := []target{{prov: "p", model: "m"}}

	t.Run("shortest wins", func(t *testing.T) {
		p, _ := stub(t, 0, "a", "b")
		p.fail(p.keys["p"][0], 429, 30*time.Second)
		p.fail(p.keys["p"][1], 429, 4*time.Second)
		if got := p.soonest(ts); got != 4*time.Second {
			t.Errorf("soonest = %v, want 4s", got)
		}
	})
	t.Run("dead keys never recover", func(t *testing.T) {
		p, _ := stub(t, 0, "a")
		p.fail(p.keys["p"][0], 401, 0)
		if got := p.soonest(ts); got != 0 {
			t.Errorf("soonest = %v, want 0 — a revoked key does not come back", got)
		}
	})
	t.Run("spent keys never recover", func(t *testing.T) {
		p, _ := stub(t, 1, "a")
		p.ok(p.pick("p"), 0)
		if got := p.soonest(ts); got != 0 {
			t.Errorf("soonest = %v, want 0 — a rollover is hours away, not seconds", got)
		}
	})
}

func TestReport(t *testing.T) {
	p, cl := stub(t, 5, "sk-super-secret")
	k := p.pick("p")
	p.ok(k, 0)
	p.fail(k, 429, time.Minute)

	b, err := json.Marshal(p.report())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-super-secret") {
		t.Fatalf("/status leaked the key: %s", b)
	}
	var got []card
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d cards, want 1", len(got))
	}
	c := got[0]
	if c.Key != Fingerprint("sk-super-secret") {
		t.Errorf("key = %q, want the hash prefix", c.Key)
	}
	if c.Left == nil || *c.Left != 4 {
		t.Errorf("left = %v, want 4 of 5", c.Left)
	}
	if want := cl.t.Add(time.Minute).UTC().Format(time.RFC3339); c.Till != want {
		t.Errorf("benched_until = %q, want %q", c.Till, want)
	}
}

func TestReportUnknownLimit(t *testing.T) {
	p, _ := stub(t, 0, "k")
	if got := p.report()[0].Left; got != nil {
		t.Errorf("left = %v, want null when rpd is unset", *got)
	}
}

func TestClientFaultIgnoresRetryHeader(t *testing.T) {
	for _, code := range []int{400, 404, 422} {
		p, cl := stub(t, 0, "k")
		k := p.pick("p")
		p.fail(k, code, 5*time.Minute)

		if k.until.After(cl.t) {
			t.Errorf("%d with a reset header benched the key until %v", code, k.until)
		}
		if p.pick("p") == nil {
			t.Errorf("%d took the key out of rotation", code)
		}
	}
}

func TestPoolUnderConcurrency(t *testing.T) {
	p, _ := stub(t, 0, "a", "b", "c")

	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			k := p.pick("p")
			if k == nil {
				return
			}
			switch i % 3 {
			case 0:
				p.ok(k, 0)
			case 1:
				p.fail(k, 500, 0)
			default:
				p.soonest([]target{{prov: "p", model: "m"}})
				p.done(k)
			}
		})
	}
	wg.Wait()

	for _, k := range p.keys["p"] {
		if k.reqs < 0 || k.errs < 0 || k.live != 0 || k.step >= len(ladder) {
			t.Errorf("key %s ended inconsistent: reqs=%d errs=%d live=%d step=%d", k.id, k.reqs, k.errs, k.live, k.step)
		}
	}
	if err := p.save(); err != nil {
		t.Fatalf("state unwritable after concurrent use: %v", err)
	}
}
