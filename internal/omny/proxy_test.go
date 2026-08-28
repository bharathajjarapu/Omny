package omny

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"
)

const rich = `{
  "model": "fast",
  "messages": [{"role": "user", "content": "a < b && c > d"}],
  "temperature": 0.30000000000000004,
  "seed": 12345678901234567890,
  "future_openai_param": {"nested": [1, 2, {"deep": true}]},
  "tools": [{"type": "function", "function": {
    "name": "lookup",
    "parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}
  }}],
  "stream_options": {"include_usage": true}
}`

func TestPeek(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		model  string
		stream bool
		bad    bool
	}{
		{name: "model and stream", body: `{"model":"fast","stream":true}`, model: "fast", stream: true},
		{name: "stream absent is false", body: `{"model":"fast"}`, model: "fast"},
		{name: "stream false", body: `{"model":"fast","stream":false}`, model: "fast"},
		{name: "rich body", body: rich, model: "fast"},
		{name: "no model", body: `{"messages":[]}`, model: ""},
		{name: "malformed", body: `{"model":`, bad: true},
		{name: "not an object", body: `["nope"]`, bad: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := peek([]byte(tt.body))
			if tt.bad {
				if err == nil {
					t.Fatal("peeked a body that is not a request")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Model != tt.model || got.Stream != tt.stream {
				t.Errorf("peek = (%q, %v), want (%q, %v)", got.Model, got.Stream, tt.model, tt.stream)
			}
		})
	}
}

func TestSwap(t *testing.T) {
	t.Parallel()
	out, err := swap([]byte(rich), "llama-3.3-70b-versatile", want{Model: "gpt-4"}, false)
	if err != nil {
		t.Fatal(err)
	}

	got, err := peek(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "llama-3.3-70b-versatile" {
		t.Errorf("model = %q, want it rewritten", got.Model)
	}

	for _, want := range []string{"0.30000000000000004", "12345678901234567890", "a < b && c > d"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output lost %q", want)
		}
	}

	var before, after map[string]any
	if err := json.Unmarshal([]byte(rich), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	delete(before, "model")
	delete(after, "model")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("body changed beyond the model name:\n got %#v\nwant %#v", after, before)
	}
}

func TestSwapRejectsNonObject(t *testing.T) {
	t.Parallel()
	if _, err := swap([]byte(`["nope"]`), "m", want{}, false); err == nil {
		t.Fatal("swapped a body that is not an object")
	}
}

func TestAfter(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		head map[string]string
		want time.Duration
	}{
		{name: "nothing offered", head: map[string]string{}},
		{name: "retry-after seconds", head: map[string]string{"Retry-After": "60"}, want: time.Minute},
		{name: "retry-after fractional seconds", head: map[string]string{"Retry-After": "1.5"}, want: 1500 * time.Millisecond},
		{
			name: "retry-after http date",
			head: map[string]string{"Retry-After": "Sat, 22 Aug 2026 12:05:00 GMT"},
			want: 5 * time.Minute,
		},
		{
			name: "a date already past offers nothing",
			head: map[string]string{"Retry-After": "Sat, 22 Aug 2026 11:00:00 GMT"},
		},
		{name: "zero seconds offers nothing", head: map[string]string{"Retry-After": "0"}},
		{name: "nonsense offers nothing", head: map[string]string{"Retry-After": "soon"}},
		{
			name: "groq reset duration",
			head: map[string]string{"X-RateLimit-Reset-Requests": "2m59.56s"},
			want: 2*time.Minute + 59560*time.Millisecond,
		},
		{
			name: "retry-after wins over the reset hint",
			head: map[string]string{"Retry-After": "30", "X-RateLimit-Reset": "600"},
			want: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			for k, v := range tt.head {
				h.Set(k, v)
			}
			if got := after(h, now); got != tt.want {
				t.Errorf("after = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnchangedBodyIsForwardedVerbatim(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.1,"tools":[]}`
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	post(t, g, body)

	if got := string(up.calls()[0].body); got != body {
		t.Errorf("body was rewritten when nothing about it changed:\n got %s\nwant %s", got, body)
	}
}

func TestAliasedBodyIsRewritten(t *testing.T) {
	up := fake(t, completion)
	g := gateway(t, &Config{
		Default:   "groq",
		Aliases:   map[string][]string{"fast": {"groq/llama-3.3-70b"}},
		Providers: map[string]*Provider{"groq": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	post(t, g, `{"model":"fast","messages":[]}`)

	if got := up.calls()[0].model; got != "llama-3.3-70b" {
		t.Errorf("upstream was sent model %q, want llama-3.3-70b", got)
	}
}
