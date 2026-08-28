package omny

import "time"

type card struct {
	Key   string `json:"key"`
	Prov  string `json:"provider"`
	Dead  bool   `json:"dead"`
	Till  string `json:"benched_until,omitempty"`
	Reqs  int    `json:"reqs"`
	Left  *int   `json:"left"`
	Spare *int   `json:"spare"`
	Errs  int    `json:"errs"`
	Last  string `json:"last,omitempty"`

	// Keep probe results optional so normal status requests stay passive.
	Probe string `json:"probe,omitempty"`
	Code  int    `json:"probe_code,omitzero"`
}

func (p *Pool) report() []card {
	now := p.now()
	out := make([]card, 0, len(p.keys))
	p.each(func(prov string, k *Key) {
		c := card{Key: k.id, Prov: prov, Dead: k.dead, Reqs: k.reqs, Errs: k.errs}
		if now.Before(k.until) {
			c.Till = k.until.UTC().Format(time.RFC3339)
		}
		if k.prov.RPD > 0 {
			c.Left = new(max(k.prov.RPD-k.reqs, 0))
		}
		if k.prov.RPM > 0 {
			c.Spare = new(max(k.prov.RPM-k.mins, 0))
		}
		if !k.last.IsZero() {
			c.Last = k.last.UTC().Format(time.RFC3339)
		}
		out = append(out, c)
	})
	return out
}

type row struct {
	Key   string `json:"key,omitempty"`
	Prov  string `json:"provider,omitempty"`
	Reqs  int    `json:"reqs"`
	Errs  int    `json:"errs"`
	Today spend  `json:"today"`
	Total spend  `json:"total"`
	Blind int    `json:"unaccounted"`
	// Use a pointer so JSON distinguishes no sample from a sub-millisecond sample.
	TTFT *int64 `json:"ttft_ms"`
}

func (r *row) add(k *Key) {
	r.Reqs, r.Errs, r.Blind = r.Reqs+k.reqs, r.Errs+k.errs, r.Blind+k.blind
	r.Today, r.Total = r.Today.add(k.toks), r.Total.add(k.life)
}

type bill struct {
	Keys  []row `json:"keys"`
	Provs []row `json:"providers"`
	All   row   `json:"total"`
}

func (p *Pool) bill() bill {
	var b bill
	p.each(func(prov string, k *Key) {
		if len(b.Provs) == 0 || b.Provs[len(b.Provs)-1].Prov != prov {
			b.Provs = append(b.Provs, row{Prov: prov, TTFT: millis(p.speed(prov))})
		}
		r := row{Key: k.id, Prov: prov, TTFT: millis(k.ttft)}
		r.add(k)
		b.Keys = append(b.Keys, r)
		b.Provs[len(b.Provs)-1].add(k)
		b.All.add(k)
	})
	return b
}

func millis(d time.Duration) *int64 {
	if d <= 0 {
		return nil
	}
	return new(d.Milliseconds())
}
