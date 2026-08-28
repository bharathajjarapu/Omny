package omny

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type trace struct {
	// Use one trace ID because concurrent attempt logs can interleave.
	id    string
	who   string
	start time.Time
	model string
	prov  string
	key   string
	tries int
	sent  string
	ttft  time.Duration
	out   string
	wait  time.Duration
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	tr := trace{id: s.mark(r), who: who(r), start: time.Now()}
	w.Header().Set("X-Request-Id", tr.id)
	s.dispatch(w, r, &tr)

	a := []any{"id", tr.id, "client", tr.who, "model", tr.model, "provider", tr.prov, "key", tr.key, "tries", tr.tries,
		"sent", tr.sent, "ms", time.Since(tr.start).Milliseconds(),
		"ttft_ms", tr.ttft.Milliseconds(), "outcome", tr.out}
	if tr.wait > 0 {
		a = append(a, "recovers_in", tr.wait.Round(time.Second).String())
	}
	s.log.Info("request", a...)
}

func (s *server) mark(r *http.Request) string {
	if got := r.Header.Get("X-Request-Id"); got != "" {
		if len(got) > 64 {
			got = got[:64]
		}
		if strings.IndexFunc(got, func(c rune) bool { return c < ' ' || c > '~' }) < 0 {
			return got
		}
	}
	return s.run + "-" + strconv.FormatUint(s.seq.Add(1), 36)
}

func (s *server) dispatch(w http.ResponseWriter, r *http.Request, tr *trace) {
	// Reserve body memory before reading so concurrent requests cannot exceed the cap.
	room := r.ContentLength
	if room < 0 || room > maxBody {
		room = maxBody
	}
	if !s.admit(room) {
		tr.out = "busy"
		refuse(w, http.StatusServiceUnavailable, "gateway is holding all the request body it can, retry shortly")
		return
	}
	defer s.release(room)

	body, err := slurp(w, r)
	if err != nil {
		tr.out = "oversized"
		refuse(w, http.StatusRequestEntityTooLarge, "request body unreadable or larger than 32MB")
		return
	}
	pl, err := readBody(body)
	if err != nil {
		tr.out = "malformed"
		refuse(w, http.StatusBadRequest, "request body is not a JSON object")
		return
	}
	if pl.Model == "" {
		tr.out = "malformed"
		refuse(w, http.StatusBadRequest, "request names no model")
		return
	}
	name := pl.Model
	tr.model = name

	// Load one snapshot so a reload cannot change targets mid-request.
	c := s.cfg.Load()

	names := append([]string{name}, c.Fallback...)
	tried := make(map[string]bool, len(names))
	var seen []target

	for i, n := range names {
		if tried[n] {
			continue
		}
		tried[n] = true
		ts := c.route(n)
		s.pool.order(ts)
		seen = append(seen, ts...)

		if s.try(w, r, ts, pl, tr) {
			return
		}
		if i == 0 {
			if d := s.pool.soonest(ts); d > 0 && d <= s.pause {
				if !nap(r.Context(), d) {
					tr.out = "gone"
					return
				}
				if s.try(w, r, ts, pl, tr) {
					return
				}
			}
		}
	}
	tr.out, tr.wait = "exhausted", s.pool.soonest(seen)
	if tr.wait > 0 {
		// Send Retry-After because clients can act on a concrete retry time.
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(tr.wait.Seconds()))))
	}
	refuse(w, http.StatusServiceUnavailable, exhausted(tr.wait))
}

func (s *server) try(w http.ResponseWriter, r *http.Request, ts []target, pl *plan, tr *trace) bool {
	done, f := s.walk(w, r, ts, pl, tr)
	if f != nil {
		tr.out = "rejected"
		refuse(w, f.code, "upstream rejected the request")
		return true
	}
	return done
}

func nap(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func exhausted(d time.Duration) string {
	if d <= 0 {
		return "all providers exhausted, and none recover soon"
	}
	return fmt.Sprintf("all providers exhausted, soonest retry in %s", d.Round(time.Second))
}

func (s *server) walk(w http.ResponseWriter, r *http.Request, ts []target, pl *plan, tr *trace) (bool, *failure) {
	tried := make(map[string]bool, len(ts))
	for _, t := range ts {
		if tried[t.prov] {
			continue
		}
		k := s.pool.pick(t.prov)
		if k == nil {
			continue
		}
		tried[t.prov] = true
		tr.tries, tr.prov, tr.key, tr.sent = tr.tries+1, t.prov, k.id, t.model

		out, f := s.attempt(r.Context(), k, t, pl)
		if f != nil {
			// Release without benching when the client cancels the attempt.
			if r.Context().Err() != nil {
				s.pool.done(k)
				tr.out = "gone"
				return true, nil //nolint:nilerr
			}
			cool, dead := s.pool.fail(k, f.code, f.retry)
			s.log.Warn("attempt failed", "id", tr.id, "provider", t.prov, "model", t.model, "key", k.id,
				"code", f.code, "benched", cool.String(), "dead", dead, "err", s.pool.scrub(f.Error()))
			if f.mine() {
				return false, f
			}
			continue
		}
		tr.ttft = out.ttft
		if s.pool.ok(k, out.ttft) {
			s.log.Info("key recovered", "id", tr.id, "provider", t.prov, "key", k.id)
		}
		err := relay(w, out, s.idle)
		s.pool.charge(k, out.cost())
		switch {
		case err == nil:
			tr.out = "ok"
		case errors.Is(err, errGone) || r.Context().Err() != nil:
			tr.out = "gone"
		default:
			// Do not fail over after commit because the client already received headers.
			tr.out = "truncated"
			cool := s.pool.sour(k)
			s.log.Error("relay failed after commit", "id", tr.id, "provider", t.prov, "key", k.id,
				"benched", cool.String(), "err", s.pool.scrub(err.Error()))
		}
		return true, nil
	}
	return false, nil
}

type failure struct {
	code  int
	retry time.Duration
	err   error
}

func (f *failure) Error() string { return f.err.Error() }
func (f *failure) Unwrap() error { return f.err }

func (f *failure) mine() bool {
	return f.code == http.StatusBadRequest || f.code == http.StatusUnprocessableEntity
}

func (s *server) attempt(ctx context.Context, k *Key, t target, pl *plan) (*result, *failure) {
	meter := pl.Stream && s.pool.talks(t.prov)
	out, f := s.once(ctx, k, t, pl, meter)
	// Retry once without injected stream_options because some providers reject it.
	if meter && f != nil && f.code == http.StatusBadRequest {
		out, f = s.once(ctx, k, t, pl, false)
		if f == nil {
			s.pool.hush(t.prov)
		}
	}
	return out, f
}

func (s *server) once(ctx context.Context, k *Key, t target, pl *plan, meter bool) (out *result, _ *failure) {
	mk, n, err := pl.send(t.model, meter)
	if err != nil {
		return nil, &failure{err: err}
	}
	// Use the first-token budget only for streams because non-streaming replies show no progress until complete.
	budget := t.at.budget(s.idle)
	if pl.Stream {
		budget = s.pool.tighten(k, t.at.budget(s.ttft), s.floor)
	}
	ctx, cancel := context.WithCancel(ctx)
	late := time.AfterFunc(budget, cancel)
	defer func() {
		if out == nil {
			late.Stop()
			cancel()
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.at.endpoint(), mk())
	if err != nil {
		return nil, &failure{err: fmt.Errorf("build request: %w", err)}
	}
	req.ContentLength = n
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(mk()), nil }
	req.Header.Set("Content-Type", "application/json")
	t.at.dress(req.Header, k.val)

	sent := time.Now()
	res, err := s.up.Do(req)
	if err != nil {
		return nil, &failure{err: fmt.Errorf("%s unreachable: %w", t.prov, err)}
	}
	if res.StatusCode >= 400 {
		res.Body.Close()
		return nil, s.flunk(res, t, fmt.Errorf("returned %d", res.StatusCode))
	}
	got, err := gate(res, pl.Stream)
	if err != nil {
		res.Body.Close()
		return nil, s.flunk(res, t, err)
	}
	// Stop the timer before returning so it cannot cancel a committed attempt.
	if !late.Stop() {
		res.Body.Close()
		return nil, s.flunk(res, t, errors.New("first token arrived as the budget expired"))
	}
	got.ttft, got.stop = time.Since(sent), cancel
	return got, nil
}

func (s *server) flunk(res *http.Response, t target, err error) *failure {
	return &failure{
		code:  res.StatusCode,
		retry: after(res.Header, s.pool.now()),
		err:   fmt.Errorf("%s: %w", t.prov, err),
	}
}

// Return OpenAI-shaped errors so clients can use their normal handling.
func refuse(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	kind := "omny_error"
	if code == http.StatusServiceUnavailable {
		kind = "omny_exhausted"
	}
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": kind},
	})
}
