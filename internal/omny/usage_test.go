package omny

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getBill(t *testing.T, g *httptest.Server) bill {
	t.Helper()
	res := ask(t, g, http.MethodGet, "/usage", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var b bill
	if err := json.Unmarshal(read(t, res), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return b
}

func TestUsageAggregates(t *testing.T) {
	a := fake(t, sse(token("hi"), meterFrame(10, 5)))
	b := fake(t, sse(token("yo"), meterFrame(3, 2)))
	_, g := stand(t, &Config{
		Default: "one",
		Aliases: map[string][]string{"x": {"one/m"}, "y": {"two/m"}},
		Providers: map[string]*Provider{
			"one": {URL: a.URL + "/v1", Keys: []string{"k1"}},
			"two": {URL: b.URL + "/v1", Keys: []string{"k2"}},
		},
	})
	post(t, g, `{"model":"x","stream":true,"messages":[]}`)
	post(t, g, `{"model":"x","stream":true,"messages":[]}`)
	post(t, g, `{"model":"y","stream":true,"messages":[]}`)

	got := getBill(t, g)
	if len(got.Keys) != 2 || len(got.Provs) != 2 {
		t.Fatalf("%d keys and %d providers, want 2 and 2", len(got.Keys), len(got.Provs))
	}
	if got.All.Total != (spend{23, 12, 35}) {
		t.Errorf("grand total %+v, want {23 12 35}", got.All.Total)
	}
	if got.All.Reqs != 3 {
		t.Errorf("reqs=%d, want 3", got.All.Reqs)
	}
	byName := map[string]row{}
	for _, r := range got.Provs {
		byName[r.Prov] = r
	}
	if byName["one"].Total != (spend{20, 10, 30}) {
		t.Errorf("provider one %+v, want {20 10 30}", byName["one"].Total)
	}
	if byName["two"].Total != (spend{3, 2, 5}) {
		t.Errorf("provider two %+v, want {3 2 5}", byName["two"].Total)
	}
}

func TestUsageHidesKeyMaterial(t *testing.T) {
	up := fake(t, sse(token("hi"), meterFrame(1, 1)))
	_, g := stand(t, &Config{
		Default:   "one",
		Providers: map[string]*Provider{"one": {URL: up.URL + "/v1", Keys: []string{"sk-secret-value"}}},
	})
	post(t, g, `{"model":"m","stream":true,"messages":[]}`)

	res := ask(t, g, http.MethodGet, "/usage", "")
	if body := string(read(t, res)); strings.Contains(body, "sk-secret-value") {
		t.Errorf("/usage leaked a key: %s", body)
	}
}

func TestUsageReportsUnaccounted(t *testing.T) {
	up := fake(t, sse(token("hi")))
	_, g := stand(t, &Config{
		Default:   "one",
		Providers: map[string]*Provider{"one": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	post(t, g, `{"model":"m","stream":true,"messages":[]}`)

	got := getBill(t, g)
	if got.All.Blind != 1 {
		t.Errorf("unaccounted=%d, want 1", got.All.Blind)
	}
	if got.All.Total != (spend{}) {
		t.Errorf("total %+v, want nothing counted", got.All.Total)
	}
}

func TestUsageOmitsUnmeasuredLatency(t *testing.T) {
	up := fake(t, completion)
	_, g := stand(t, &Config{
		Default:   "one",
		Providers: map[string]*Provider{"one": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	res := ask(t, g, http.MethodGet, "/usage", "")
	var raw map[string]any
	if err := json.Unmarshal(read(t, res), &raw); err != nil {
		t.Fatal(err)
	}
	keys := raw["keys"].([]any)
	if got, ok := keys[0].(map[string]any)["ttft_ms"]; !ok || got != nil {
		t.Errorf("an unused key reported latency %v, want null", got)
	}
}

func TestUsageNeedsTheToken(t *testing.T) {
	up := fake(t, completion)
	_, g := stand(t, &Config{
		Default:   "one",
		Providers: map[string]*Provider{"one": {URL: up.URL + "/v1", Keys: []string{"k"}}},
	})
	req, _ := http.NewRequest(http.MethodGet, g.URL+"/usage", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d without a token, want 401", res.StatusCode)
	}
}
