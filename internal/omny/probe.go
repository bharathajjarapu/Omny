package omny

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
)

const (
	shallow = "models"
	deep    = "chat"
)

const spark = `{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`

// Bound probe concurrency so diagnostics do not create a traffic burst.
const fanout = 16

type shot struct {
	said string
	code int
}

func (s *server) probe(ctx context.Context, c *Config, depth string) map[string]shot {
	ask := c.asked()

	var mu sync.Mutex
	out := make(map[string]shot, len(c.Providers))
	sem := make(chan struct{}, fanout)
	var wg sync.WaitGroup

	for name, at := range c.Providers {
		for _, sl := range at.slots(name) {
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()

				got := s.shoot(ctx, at, ask[name], depth, sl.val)
				mu.Lock()
				out[sl.id] = got
				mu.Unlock()
			})
		}
	}
	wg.Wait()
	return out
}

func (s *server) shoot(ctx context.Context, at *Provider, model, depth, key string) shot {
	if depth == deep && model == "" {
		return shot{said: "unprobeable"}
	}
	// Use the provider budget for deep probes because first tokens can be slow.
	wait := s.patience
	if depth == deep {
		wait = at.budget(wait)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	req, err := at.ping(ctx, depth, model)
	if err != nil {
		return shot{said: "unreachable"}
	}
	at.dress(req.Header, key)

	// Reuse the relay client because some providers reject unfamiliar clients.
	res, err := s.up.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return shot{said: "timeout"}
		}
		return shot{said: "unreachable"}
	}
	defer res.Body.Close()
	// Drain a small response so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))

	if res.StatusCode >= 300 {
		return shot{said: "rejected", code: res.StatusCode}
	}
	return shot{said: "ok", code: res.StatusCode}
}

func (p *Provider) ping(ctx context.Context, depth, model string) (*http.Request, error) {
	if depth == shallow {
		return http.NewRequestWithContext(ctx, http.MethodGet, p.catalog(), nil)
	}
	body := strings.NewReader(fmt.Sprintf(spark, model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), body)
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, err
}

func (c *Config) asked() map[string]string {
	m := make(map[string]string, len(c.Providers))
	for _, a := range slices.Sorted(maps.Keys(c.Aliases)) {
		for _, t := range c.route(a) {
			if m[t.prov] == "" {
				m[t.prov] = t.model
			}
		}
	}
	return m
}
