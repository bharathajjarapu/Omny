package omny

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

func sent(t *testing.T, p *plan, model string, meter bool) ([]byte, map[string]any) {
	t.Helper()
	mk, n, err := p.send(model, meter)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	b, err := io.ReadAll(mk())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if int64(len(b)) != n {
		t.Fatalf("send promised %d bytes and produced %d; Content-Length would be a lie", n, len(b))
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("spliced body is not valid JSON: %v\n%s", err, b)
	}
	return b, m
}

func TestSpliceAgreesWithSwap(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"model first", `{"model":"a","messages":[{"role":"user","content":"hi"}]}`},
		{"model last", `{"messages":[{"role":"user","content":"hi"}],"model":"a"}`},
		{"model middle", `{"n":1,"model":"a","messages":[]}`},
		{"whitespace", "{\n  \"model\" : \"a\" ,\n  \"messages\" : [ ]\n}"},
		{"nested model key", `{"model":"a","metadata":{"model":"decoy"},"messages":[]}`},
		{"escapes", `{"model":"a","messages":[{"content":"quote \" and \\ and é"}]}`},
		{"unicode", `{"model":"a","messages":[{"content":"héllo 世界 🌍"}]}`},
		{"float precision", `{"model":"a","temperature":0.123456789012345,"top_p":0.95}`},
		{"tools", `{"model":"a","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`},
		{"slash in name", `{"model":"a","messages":[]}`},
		{"empty messages", `{"model":"a","messages":[]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, meter := range []bool{false, true} {
				p, err := readBody([]byte(c.body))
				if err != nil {
					t.Fatalf("readBody: %v", err)
				}
				if p.hi == 0 {
					t.Fatalf("locate declined a body it should handle: %s", c.body)
				}
				_, fast := sent(t, p, "provider/real-name", meter)

				slow, err := swap([]byte(c.body), "provider/real-name", p.want, meter)
				if err != nil {
					t.Fatalf("swap: %v", err)
				}
				var want map[string]any
				if err := json.Unmarshal(slow, &want); err != nil {
					t.Fatalf("swap output invalid: %v", err)
				}
				if !reflect.DeepEqual(fast, want) {
					t.Errorf("meter=%v: splice and swap disagree\n splice %v\n swap   %v", meter, fast, want)
				}
			}
		})
	}
}

func TestSplicePreservesEveryOtherByte(t *testing.T) {
	body := `{"model":"alias","temperature":0.123456789012345,"messages":[{"role":"user","content":"héllo \" 世界"}],"tools":[{"a":[1,2,3]}]}`
	p, err := readBody([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := sent(t, p, "real", false)
	want := strings.Replace(body, `"model":"alias"`, `"model":"real"`, 1)
	if string(got) != want {
		t.Errorf("splice altered bytes it had no business touching\n got  %s\n want %s", got, want)
	}
}

func TestSpliceAddsUsageOnlyWhenAsked(t *testing.T) {
	for _, c := range []struct {
		name, body string
		meter, add bool
	}{
		{"streaming asks for it", `{"model":"a","stream":true}`, true, true},
		{"not metering leaves it alone", `{"model":"a","stream":true}`, false, false},
		{"the caller's own is never overridden", `{"model":"a","stream":true,"stream_options":{"include_usage":false}}`, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := readBody([]byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			_, m := sent(t, p, "real", c.meter)
			o, ok := m["stream_options"].(map[string]any)
			switch {
			case c.add && (!ok || o["include_usage"] != true):
				t.Errorf("stream_options was not added: %v", m["stream_options"])
			case !c.add && c.meter && o["include_usage"] != false:
				t.Errorf("the caller's own stream_options was overridden: %v", m["stream_options"])
			case !c.add && !c.meter && ok:
				t.Errorf("stream_options appeared unasked: %v", m["stream_options"])
			}
		})
	}
}

func TestUnchangedNameIsTheSameBytes(t *testing.T) {
	body := `{"model":"same","messages":[]}`
	p, err := readBody([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := sent(t, p, "same", false)
	if string(got) != body {
		t.Errorf("body was rewritten for no reason\n got %s\n want %s", got, body)
	}
}

func TestOddShapesFallBackRatherThanFail(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"top level array", `[{"model":"a"}]`},
		{"no model at all", `{"messages":[]}`},
		{"trailing garbage", `{"model":"a"} trailing`},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := readBody([]byte(c.body))
			if err != nil {
				return
			}
			if p.hi != 0 {
				t.Fatalf("locate claimed a shape it should have declined: %s", c.body)
			}
		})
	}
}

func TestSendLengthMatchesBytes(t *testing.T) {
	for _, body := range []string{
		`{"model":"a","messages":[]}`,
		`{"messages":[],"model":"a"}`,
		`{"model":"a","stream":true}`,
	} {
		for _, meter := range []bool{false, true} {
			for _, name := range []string{"a", "much-longer-provider-model-name", "x"} {
				p, err := readBody([]byte(body))
				if err != nil {
					t.Fatal(err)
				}
				sent(t, p, name, meter)
			}
		}
	}
}

func TestDecliningToSpliceChangesNothingButSpeed(t *testing.T) {
	for _, body := range []string{
		`{"model":"alias","stream":true,"messages":[{"content":"hi"}]}`,
		`{"messages":[],"model":"alias","temperature":0.5}`,
		`{"model":"alias","stream":true,"stream_options":{"include_usage":false}}`,
	} {
		fast, err := readBody([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if fast.hi == 0 {
			t.Fatalf("wanted a spliceable body, got one locate declined: %s", body)
		}
		declined := *fast
		declined.hi = 0

		for _, meter := range []bool{false, true} {
			_, a := sent(t, fast, "real", meter)
			_, b := sent(t, &declined, "real", meter)
			if !reflect.DeepEqual(a, b) {
				t.Errorf("meter=%v: the fallback sent a different request\n splice %v\n fallback %v", meter, a, b)
			}
		}
	}
}
