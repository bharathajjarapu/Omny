package omny

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

type state struct {
	Day  int              `json:"day"`
	Keys map[string]tally `json:"keys"`
}

type tally struct {
	Reqs  int           `json:"reqs"`
	Errs  int           `json:"errs"`
	Toks  spend         `json:"toks"`
	Blind int           `json:"blind"`
	TTFT  time.Duration `json:"ttft"`
	Life  spend         `json:"life"`
}

func (p *Pool) restore() error {
	b, err := os.ReadFile(p.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("parse %s: %w", p.path, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Ignore saved daily counters from another day because they cannot apply to today's quota.
	today := s.Day == p.day
	for _, ks := range p.keys {
		for _, k := range ks {
			t := s.Keys[k.id]
			k.ttft, k.life = t.TTFT, t.Life
			if today {
				k.reqs, k.errs, k.toks, k.blind = t.Reqs, t.Errs, t.Toks, t.Blind
			}
		}
	}
	return nil
}

func (p *Pool) save() error {
	p.mu.Lock()
	s := state{Day: p.day, Keys: make(map[string]tally, len(p.keys))}
	for _, ks := range p.keys {
		for _, k := range ks {
			s.Keys[k.id] = tally{Reqs: k.reqs, Errs: k.errs, Toks: k.toks,
				Blind: k.blind, TTFT: k.ttft, Life: k.life}
		}
	}
	p.mu.Unlock()

	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	return Replace(p.path, b, nil)
}

// Write to a private temporary file and rename it so a crash cannot leave partial state.
func Replace(path string, b []byte, vet func(tmp string) error) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if vet != nil {
		if err := vet(tmp); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func (p *Pool) flush(stop <-chan struct{}, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-stop:
			// Flush once after stop so the final counters reach disk.
			if err := p.save(); err != nil {
				log.Error("final state flush", "err", err)
			}
			return
		}
		if err := p.save(); err != nil {
			log.Error("state flush", "err", err)
		}
	}
}
