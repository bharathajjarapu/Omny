package omny

import (
	"math/bits"
	"slices"
	"time"
)

func (k *Key) measure(d time.Duration) {
	if d <= 0 {
		return
	}
	if k.ttft == 0 {
		k.ttft = d
		return
	}
	k.ttft = (3*d + 7*k.ttft) / 10
}

func (p *Pool) speed(prov string) time.Duration {
	var sum time.Duration
	n := 0
	for _, k := range p.keys[prov] {
		if k.ttft > 0 && p.free(k) {
			sum, n = sum+k.ttft, n+1
		}
	}
	if n == 0 {
		return 0
	}
	return sum / time.Duration(n)
}

// Tiered ordering keeps the sort comparator transitive while tolerating timing noise.
const grain = 250 * time.Millisecond

func tier(d time.Duration) int {
	if d <= grain {
		return 0
	}
	return bits.Len64(uint64(d / grain))
}

func (p *Pool) order(ts []target) {
	if len(ts) < 2 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roll()

	type ranked struct {
		t target
		d time.Duration
	}
	at := make([]int, 0, len(ts))
	rs := make([]ranked, 0, len(ts))
	for i, t := range ts {
		if d := p.speed(t.prov); d > 0 {
			at = append(at, i)
			rs = append(rs, ranked{t, d})
		}
	}
	if len(rs) < 2 {
		return
	}
	// Stable sorting preserves config order within a tier.
	slices.SortStableFunc(rs, func(a, b ranked) int { return tier(a.d) - tier(b.d) })
	for j, r := range rs {
		ts[at[j]] = r.t
	}
}

// Tightening only shortens the configured budget and never removes its floor.
const slack = 3

func (p *Pool) tighten(k *Key, want, floor time.Duration) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k.ttft == 0 {
		return want
	}
	return min(want, max(floor, slack*k.ttft))
}
