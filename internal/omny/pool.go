package omny

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type Key struct {
	val  string
	prov *Provider
	id   string

	until time.Time
	step  int
	dead  bool

	reqs int
	errs int
	mins int
	live int
	last time.Time

	ttft time.Duration

	toks  spend
	life  spend
	blind int
}

// Count live reservations so concurrent requests cannot overshoot the cap.
func (k *Key) spent() bool {
	return k.prov.RPD > 0 && k.reqs+k.live >= k.prov.RPD
}

func (k *Key) capped() bool {
	return k.prov.RPM > 0 && k.mins+k.live >= k.prov.RPM
}

type slot struct{ id, val string }

func (p *Provider) slots(name string) []slot {
	if p.Keyless {
		// Give keyless providers distinct identities because an empty secret is shared by all of them.
		return []slot{{id: Fingerprint("keyless/" + name)}}
	}
	ss := make([]slot, len(p.Keys))
	for i, v := range p.Keys {
		ss[i] = slot{Fingerprint(v), v}
	}
	return ss
}

type Pool struct {
	mu   sync.Mutex
	keys map[string][]*Key
	rr   map[string]int
	day  int
	min  int64
	path string
	seen map[string]string
	mask *strings.Replacer
	mute map[string]bool
	now  func() time.Time
}

func pool(c *Config) *Pool {
	p := &Pool{now: time.Now}
	p.day, p.min = p.now().UTC().YearDay(), p.now().Unix()/60
	p.adopt(c)
	return p
}

// Reuse key state by fingerprint while rebuilding the provider slice.
func (p *Pool) adopt(c *Config) {
	p.mu.Lock()
	defer p.mu.Unlock()

	old := make(map[string]*Key, len(p.keys))
	for _, ks := range p.keys {
		for _, k := range ks {
			old[k.id] = k
		}
	}
	keys := make(map[string][]*Key, len(c.Providers))
	if p.seen == nil {
		p.seen = make(map[string]string)
	}
	for name, prov := range c.Providers {
		ss := prov.slots(name)
		ks := make([]*Key, 0, len(ss))
		for _, sl := range ss {
			k := old[sl.id]
			if k == nil {
				k = &Key{val: sl.val, id: sl.id}
			}
			// Update quota metadata only because request targets keep their snapshot provider.
			k.prov = prov
			ks = append(ks, k)
			if sl.val != "" {
				p.seen[sl.val] = sl.id
			}
		}
		keys[name] = ks
	}
	p.keys = keys
	// Reset cursors because a reload may shorten provider slices.
	p.rr = make(map[string]int, len(c.Providers))
	if p.mute == nil {
		p.mute = make(map[string]bool)
	}
	p.path = c.State

	// Keep removed secrets in the scrubber while in-flight requests can still log them.
	swaps := make([]string, 0, 2*len(p.seen))
	for v, id := range p.seen {
		swaps = append(swaps, v, id)
	}
	p.mask = strings.NewReplacer(swaps...)
}

func (p *Pool) scrub(s string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mask.Replace(s)
}

func Fingerprint(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:4])
}

// Reserve the key under the lock before upstream I/O begins.
func (p *Pool) pick(prov string) *Key {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()

	ks := p.keys[prov]
	for range ks {
		k := ks[p.rr[prov]]
		p.rr[prov] = (p.rr[prov] + 1) % len(ks)
		if p.free(k) {
			k.live++
			return k
		}
	}
	return nil
}

func (p *Pool) free(k *Key) bool {
	return !k.dead && !k.spent() && !k.capped() && !p.now().Before(k.until)
}

func blameless(code int) bool {
	return code >= 400 && code < 500 &&
		code != http.StatusTooManyRequests && code != http.StatusPaymentRequired
}

var ladder = [...]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 24 * time.Hour}

func (p *Pool) fail(k *Key, code int, retry time.Duration) (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()

	k.live--
	k.errs++
	return p.bench(k, code, retry)
}

// Do not release live here because ok already released this attempt.
func (p *Pool) sour(k *Key) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()

	k.errs++
	d, _ := p.bench(k, 0, 0)
	return d
}

// Release the reservation when the client leaves before the attempt resolves.
func (p *Pool) done(k *Key) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.live--
}

func (p *Pool) bench(k *Key, code int, retry time.Duration) (time.Duration, bool) {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		k.dead = true
		return 0, true
	// Check blameless errors before reset headers so a 400 cannot bench a good key.
	case blameless(code):
		return 0, false
	case retry > 0:
		k.until = p.now().Add(retry)
		return retry, false
	default:
		d := ladder[k.step]
		k.until = p.now().Add(d)
		k.step = min(k.step+1, len(ladder)-1)
		return d, false
	}
}

// Record missing usage separately so totals do not imply zero cost.
func (p *Pool) charge(k *Key, s *spend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()

	if s == nil {
		k.blind++
		return
	}
	k.toks, k.life = k.toks.add(*s), k.life.add(*s)
}

// Remember unsupported usage fields so each request avoids another round trip.
func (p *Pool) talks(prov string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.mute[prov]
}

func (p *Pool) hush(prov string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mute[prov] = true
}

func (p *Pool) ok(k *Key, ttft time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()

	back := !k.until.IsZero()
	k.until, k.step = time.Time{}, 0
	k.live--
	k.reqs++
	k.mins++
	k.last = p.now()
	k.measure(ttft)
	return back
}

func (p *Pool) ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()

	for _, ks := range p.keys {
		for _, k := range ks {
			if p.free(k) {
				return true
			}
		}
	}
	return false
}

func (p *Pool) soonest(ts []target) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()

	now, best := p.now(), time.Duration(0)
	for _, t := range ts {
		for _, k := range p.keys[t.prov] {
			if k.dead || k.spent() {
				continue
			}
			d := k.until.Sub(now)
			// Include minute-cap recovery because it can precede the daily limit.
			if k.capped() {
				d = max(d, time.Duration(60-now.Unix()%60)*time.Second)
			}
			if d > 0 && (best == 0 || d < best) {
				best = d
			}
		}
	}
	return best
}

// Roll counters on access because a timer could leave routing with stale limits.
func (p *Pool) roll() {
	now := p.now()
	if d := now.UTC().YearDay(); d != p.day {
		p.day = d
		for _, ks := range p.keys {
			for _, k := range ks {
				k.reqs, k.errs, k.toks, k.blind = 0, 0, spend{}, 0
			}
		}
	}
	if m := now.Unix() / 60; m != p.min {
		p.min = m
		for _, ks := range p.keys {
			for _, k := range ks {
				k.mins = 0
			}
		}
	}
}

func (p *Pool) each(f func(prov string, k *Key)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()
	for _, prov := range slices.Sorted(maps.Keys(p.keys)) {
		for _, k := range p.keys[prov] {
			f(prov, k)
		}
	}
}
