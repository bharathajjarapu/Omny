package omny

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type server struct {
	// Keep one config snapshot per request so reloads cannot split a request across versions.
	cfg  atomic.Pointer[Config]
	pool *Pool
	// Do not set a total client timeout because a valid generation may be slow.
	up  *http.Client
	log *slog.Logger

	ttft     time.Duration
	idle     time.Duration
	pause    time.Duration
	patience time.Duration
	floor    time.Duration

	// Reserve body memory before reading so concurrent requests cannot exceed the cap.
	hold int64
	held atomic.Int64

	// Prefix generated IDs with a run value so they stay unique across restarts.
	run string
	seq atomic.Uint64
}

func (s *server) admit(n int64) bool {
	for {
		held := s.held.Load()
		if held+n > s.hold {
			return false
		}
		if s.held.CompareAndSwap(held, held+n) {
			return true
		}
	}
}

func (s *server) release(n int64) { s.held.Add(-n) }

func serve(c *Config) *server {
	s := &server{
		pool:     pool(c),
		log:      slog.Default(),
		run:      strconv.FormatUint(uint64(rand.Uint32()), 36),
		ttft:     15 * time.Second,
		idle:     60 * time.Second,
		pause:    5 * time.Second,
		patience: 20 * time.Second,
		floor:    2 * time.Second,
		hold:     8 * maxBody,

		up: &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
				// Keep dialing bounded separately because model generation can take longer.
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
	s.cfg.Store(c)
	return s
}

// Swap only after validation so a bad reload leaves the active config unchanged.
func (s *server) reload(path string) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	if old := s.cfg.Load(); c.Listen != old.Listen {
		s.log.Warn("listen address changed but the socket is already bound; restart to apply",
			"running", old.Listen, "file", c.Listen)
	}
	s.pool.adopt(c)
	s.cfg.Store(c)
	return nil
}

// Leave WriteTimeout unset because valid streaming generations have no fixed total time.
func (s *server) listener() *http.Server {
	return &http.Server{
		Addr:              s.cfg.Load().Listen,
		Handler:           s.mux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (s *server) mux() http.Handler {
	m := http.NewServeMux()
	m.Handle("GET /healthz", s.guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })))
	m.Handle("GET /readyz", s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.pool.ready() {
			refuse(w, http.StatusServiceUnavailable, "no key can serve right now")
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})))
	m.Handle("POST /v1/chat/completions", s.guard(http.HandlerFunc(s.chat)))
	m.Handle("GET /v1/models", s.guard(http.HandlerFunc(s.models)))
	m.Handle("GET /status", s.guard(http.HandlerFunc(s.status)))
	m.Handle("GET /usage", s.guard(http.HandlerFunc(s.usage)))
	return s.shield(m)
}

// Convert panics to error responses so clients do not receive EOF.
func (s *server) shield(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// Re-panic ErrAbortHandler because it intentionally closes the connection.
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			s.log.Error("panic serving request", "path", r.URL.Path,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			refuse(w, http.StatusInternalServerError, "internal error")
		}()
		h.ServeHTTP(w, r)
	})
}

type caller struct{}

func who(r *http.Request) string {
	name, _ := r.Context().Value(caller{}).(string)
	return name
}

// Compare bearer tokens in constant time so the matched token is not leaked by timing.
func (s *server) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		name := ""
		if ok {
			name = s.cfg.Load().who(got)
		}
		if name == "" {
			s.log.Warn("unauthorized", "path", r.URL.Path, "from", r.RemoteAddr)
			refuse(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), caller{}, name)))
	})
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	aliases := s.cfg.Load().Aliases
	ids := make(map[string]bool, len(aliases))
	for a, es := range aliases {
		ids[a] = true
		for _, e := range es {
			ids[e] = true
		}
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "omny"})
	}
	send(w, map[string]any{"object": "list", "data": data})
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	cards := s.pool.report()
	switch depth := r.URL.Query().Get("probe"); depth {
	case "":
	case shallow, deep:
		if depth == deep {
			// Exclude diagnostic requests so probing does not consume routing quota.
			w.Header().Set("X-Omny-Probe-Cost", "one request per key, not counted against left/spare")
			s.log.Info("probing", "depth", depth, "keys", len(cards), "spends_quota", true)
		}
		got := s.probe(r.Context(), s.cfg.Load(), depth)
		for i := range cards {
			if sh, ok := got[cards[i].Key]; ok {
				cards[i].Probe, cards[i].Code = sh.said, sh.code
			}
		}
	default:
		refuse(w, http.StatusBadRequest,
			`probe must be "`+shallow+`" (free) or "`+deep+`" (spends one request per key)`)
		return
	}
	send(w, cards)
}

func (s *server) usage(w http.ResponseWriter, _ *http.Request) { send(w, s.pool.bill()) }

func send(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
