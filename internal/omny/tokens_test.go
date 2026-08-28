package omny

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func meterFrame(in, out int) string {
	return `{"id":"c1","object":"chat.completion.chunk","choices":[],` +
		`"usage":{"prompt_tokens":` + itoa(in) + `,"completion_tokens":` + itoa(out) +
		`,"total_tokens":` + itoa(in+out) + `}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func only(t *testing.T, p *Pool, prov string) *Key {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	ks := p.keys[prov]
	if len(ks) != 1 {
		t.Fatalf("provider %s has %d keys, want 1", prov, len(ks))
	}
	return ks[0]
}

func TestSniffFindsUsage(t *testing.T) {
	cases := []struct {
		name string
		feed []string
		want *spend
	}{
		{"a whole frame in one read",
			[]string{"data: " + meterFrame(9, 3) + "\n\ndata: [DONE]\n\n"},
			&spend{9, 3, 12}},
		{"split across reads",
			[]string{`data: {"choices":[],"us`, `age":{"prompt_tokens":4,"completion_tokens":6,`,
				`"total_tokens":10}}` + "\n\ndata: [DONE]\n\n"},
			&spend{4, 6, 10}},
		{"a provider that omits the total",
			[]string{`{"usage":{"prompt_tokens":2,"completion_tokens":5}}`},
			&spend{2, 5, 7}},
		{"null in every chunk but the last",
			[]string{`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\n",
				`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}` + "\n\n",
				"data: [DONE]\n\n"},
			&spend{9, 3, 12}},
		{"null, with an object after it in the same frame",
			[]string{`data: {"usage":null,"choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"},
			nil},
		{"null and never anything else",
			[]string{`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\ndata: [DONE]\n\n"},
			nil},
		{"no usage at all",
			[]string{"data: " + token("hi") + "\n\ndata: [DONE]\n\n"},
			nil},
		{"the frame was cut short",
			[]string{`{"usage":{"prompt_tokens":2,"completion`},
			nil},
		{"the word in message content is not a count",
			[]string{`data: {"choices":[{"delta":{"content":"the \"usage\" of it"}}]}`},
			nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s sniff
			for _, p := range c.feed {
				s.feed([]byte(p))
			}
			got := s.count()
			switch {
			case c.want == nil && got != nil:
				t.Errorf("counted %+v, want nothing", *got)
			case c.want != nil && got == nil:
				t.Errorf("counted nothing, want %+v", *c.want)
			case c.want != nil && *got != *c.want:
				t.Errorf("counted %+v, want %+v", *got, *c.want)
			}
		})
	}
}

func TestSniffKeepsTheEnd(t *testing.T) {
	var s sniff
	for range 50 {
		s.feed([]byte("data: " + token(strings.Repeat("x", 400)) + "\n\n"))
	}
	s.feed([]byte("data: " + meterFrame(11, 22) + "\n\ndata: [DONE]\n\n"))
	got := s.count()
	if got == nil || *got != (spend{11, 22, 33}) {
		t.Errorf("counted %v after 50 frames, want {11 22 33}", got)
	}
}

func TestStreamTokensCounted(t *testing.T) {
	up := fake(t, sse(token("hi"), meterFrame(9, 3)))
	srv, g := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	if res, b := post(t, g, `{"model":"m","stream":true,"messages":[]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	k := only(t, srv.pool, "groq")
	if k.toks != (spend{9, 3, 12}) || k.life != (spend{9, 3, 12}) {
		t.Errorf("today %+v lifetime %+v, want {9 3 12} each", k.toks, k.life)
	}
	if k.blind != 0 {
		t.Errorf("blind=%d, want 0 — the count was right there", k.blind)
	}
}

func TestNonStreamTokensCounted(t *testing.T) {
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"hi"}}],`+
			`"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
	})
	srv, g := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	if res, b := post(t, g, `{"model":"m","messages":[]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	if k := only(t, srv.pool, "groq"); k.toks != (spend{7, 2, 9}) {
		t.Errorf("today %+v, want {7 2 9}", k.toks)
	}
}

func TestMissingUsageIsUnaccounted(t *testing.T) {
	up := fake(t, sse(token("hi")))
	srv, g := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	post(t, g, `{"model":"m","stream":true,"messages":[]}`)
	k := only(t, srv.pool, "groq")
	if k.blind != 1 {
		t.Errorf("blind=%d, want 1", k.blind)
	}
	if k.toks != (spend{}) {
		t.Errorf("today %+v, want nothing counted", k.toks)
	}
}

func TestMeterIsAskedForOnlyWhenStreaming(t *testing.T) {
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if q, _ := peek(mustRead(t, r)); q.Stream {
			sse(token("hi"), meterFrame(1, 1))(w, r)
			return
		}
		completion(w, r)
	})
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	post(t, g, `{"model":"m","messages":[]}`)
	post(t, g, `{"model":"m","stream":true,"messages":[]}`)

	calls := up.calls()
	if bytes.Contains(calls[0].body, []byte("stream_options")) {
		t.Errorf("a non-streaming request carried stream_options: %s", calls[0].body)
	}
	if !bytes.Contains(calls[1].body, []byte(`"include_usage":true`)) {
		t.Errorf("a streaming request did not ask for a count: %s", calls[1].body)
	}
}

func TestClientStreamOptionsSurvive(t *testing.T) {
	up := fake(t, sse(token("hi")))
	g := gateway(t, &Config{
		Default:   "groq",
		Aliases:   map[string][]string{"fast": {"groq/m"}},
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	post(t, g, `{"model":"fast","stream":true,"stream_options":{"include_usage":false},"messages":[]}`)

	got := up.calls()[0].body
	if !bytes.Contains(got, []byte(`"include_usage":false`)) {
		t.Errorf("the caller's stream_options was overwritten: %s", got)
	}
}

func TestMeterSelfHeals(t *testing.T) {
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if bytes.Contains(mustRead(t, r), []byte("stream_options")) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"unknown field stream_options"}}`)
			return
		}
		sse(token("hi"))(w, r)
	})
	srv, g := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	if res, b := post(t, g, `{"model":"m","stream":true,"messages":[]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("the retry without the field did not serve: %d %s", res.StatusCode, b)
	}
	if n := len(up.calls()); n != 2 {
		t.Fatalf("%d calls, want 2 — one rejected, one retried", n)
	}
	if res, b := post(t, g, `{"model":"m","stream":true,"messages":[]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	if n := len(up.calls()); n != 3 {
		t.Errorf("%d calls, want 3 — the provider was asked again after refusing", n)
	}
	if k := only(t, srv.pool, "groq"); k.dead || !k.until.IsZero() {
		t.Errorf("the retried 400 cost the key a bench: dead=%v until=%v", k.dead, k.until)
	}
}

func mustRead(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("upstream read: %v", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

func TestTokenCountersRoll(t *testing.T) {
	p, cl := stub(t, 0, "k")
	k := p.pick("p")
	p.charge(k, &spend{5, 5, 10})
	p.charge(k, nil)

	cl.tick(25 * time.Hour)
	p.charge(k, &spend{1, 1, 2})

	if k.toks != (spend{1, 1, 2}) {
		t.Errorf("today %+v, want {1 1 2} — yesterday's tokens carried over", k.toks)
	}
	if k.life != (spend{6, 6, 12}) {
		t.Errorf("lifetime %+v, want {6 6 12} — a total that restarts at midnight is not one", k.life)
	}
	if k.blind != 0 {
		t.Errorf("blind=%d, want 0 — yesterday's unaccounted carried over", k.blind)
	}
}

func TestAClientsOwn400DoesNotMuteTheProvider(t *testing.T) {
	up := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"your messages array is empty"}}`)
	})
	srv, g := stand(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	post(t, g, `{"model":"m","stream":true,"messages":[]}`)

	if !srv.pool.talks("groq") {
		t.Error("a 400 the caller earned muted the provider's token counting")
	}
	if n := len(up.calls()); n != 2 {
		t.Errorf("%d calls, want 2 — one metered, one retried to find out whose fault it was", n)
	}
}
