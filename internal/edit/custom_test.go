package edit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathajjarapu/omny/internal/omny"
)

func TestUrlCreatesTheProvider(t *testing.T) {
	path := menu(t)
	before := slurp(t, path)

	if _, err := sh(t, path, "add", "mine", "sk-mine",
		"-url", "https://api.example.invalid/v1", "-model", "llama-4-scout"); err != nil {
		t.Fatal(err)
	}

	c, err := omny.Load(path)
	if err != nil {
		t.Fatalf("the edited config does not load: %v", err)
	}
	p := c.Providers["mine"]
	switch {
	case p == nil:
		t.Fatal("the provider was not created")
	case p.URL != "https://api.example.invalid/v1":
		t.Errorf("url %q", p.URL)
	case len(p.Keys) != 1 || p.Keys[0] != "sk-mine":
		t.Errorf("keys %v, want the one key", p.Keys)
	}
	if got := c.Aliases["mine"]; len(got) != 1 || got[0] != "mine/llama-4-scout" {
		t.Errorf("alias mine is %v, want [mine/llama-4-scout]", got)
	}
	after := slurp(t, path)
	if !strings.Contains(after, "\n  mine:\n    url: https://api.example.invalid/v1\n") {
		t.Errorf("the block is not where a reader would look:\n%s", tailOf(after, 12))
	}
	if n, m := strings.Count(before, "#"), strings.Count(after, "#"); n != m {
		t.Errorf("%d comment markers became %d", n, m)
	}
	if n, m := len(strings.Split(before, "\n")), len(strings.Split(after, "\n")); m != n+4 {
		t.Errorf("the file went from %d lines to %d, want %d", n, m, n+4)
	}
}

func TestFlagsMayFollowTheName(t *testing.T) {
	for _, args := range [][]string{
		{"add", "mine", "sk-mine", "-url", "https://api.example.invalid/v1"},
		{"add", "-url", "https://api.example.invalid/v1", "mine", "sk-mine"},
		{"add", "mine", "-url", "https://api.example.invalid/v1", "sk-mine"},
	} {
		t.Run(strings.Join(args[1:3], " "), func(t *testing.T) {
			path := menu(t)
			if _, err := sh(t, path, args...); err != nil {
				t.Fatal(err)
			}
			c, err := omny.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if p := c.Providers["mine"]; p == nil || len(p.Keys) != 1 {
				t.Errorf("provider %+v, want one created with one key", p)
			}
		})
	}
}

func TestPastedEndpointIsRefused(t *testing.T) {
	path := menu(t)
	before := slurp(t, path)
	_, err := sh(t, path, "add", "mine", "sk-mine",
		"-url", "https://api.example.invalid/v1/chat/completions")
	if err == nil {
		t.Fatal("an endpoint URL was accepted as a base URL")
	}
	if !strings.Contains(err.Error(), "base URL") {
		t.Errorf("error %q does not say what to give instead", err)
	}
	if slurp(t, path) != before {
		t.Error("the rejected edit was installed anyway")
	}
}

func TestUrlOnAProviderThatExists(t *testing.T) {
	cases := []struct {
		name string
		url  string
		keys int
	}{
		{name: "the same url just adds the key", url: "https://api.example.invalid/v1", keys: 2},
		{name: "a different url is refused", url: "https://elsewhere.invalid/v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := menu(t)
			if _, err := sh(t, path, "add", "mine", "sk-one", "-url", "https://api.example.invalid/v1"); err != nil {
				t.Fatal(err)
			}
			was := slurp(t, path)

			_, err := sh(t, path, "add", "mine", "sk-two", "-url", c.url)
			if c.keys == 0 {
				if err == nil {
					t.Fatal("a provider was silently re-pointed")
				}
				if slurp(t, path) != was {
					t.Error("the rejected edit was installed anyway")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := omny.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(cfg.Providers["mine"].Keys); got != c.keys {
				t.Errorf("%d keys, want %d", got, c.keys)
			}
			if n := strings.Count(slurp(t, path), "\n  mine:"); n != 1 {
				t.Errorf("the provider block appears %d times", n)
			}
		})
	}
}

func TestTheModelIsPinnedOnce(t *testing.T) {
	path := menu(t)
	for i, key := range []string{"sk-one", "sk-two"} {
		if _, err := sh(t, path, "add", "mine", key,
			"-url", "https://api.example.invalid/v1", "-model", "llama-4-scout"); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	c, err := omny.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Aliases["mine"]; len(got) != 1 {
		t.Errorf("alias mine is %v, want the one entry", got)
	}
}

func TestASecondModelJoinsTheAlias(t *testing.T) {
	path := menu(t)
	if _, err := sh(t, path, "add", "mine", "sk-one",
		"-url", "https://api.example.invalid/v1", "-model", "small"); err != nil {
		t.Fatal(err)
	}
	if _, err := sh(t, path, "add", "mine", "sk-two", "-model", "large"); err != nil {
		t.Fatal(err)
	}
	c, err := omny.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mine/small", "mine/large"}
	if got := c.Aliases["mine"]; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("alias mine is %v, want %v", got, want)
	}
}

func TestAnUnknownProviderPointsAtUrl(t *testing.T) {
	path := menu(t)
	_, err := sh(t, path, "add", "nosuch", "sk-one")
	if err == nil || !strings.Contains(err.Error(), "-url") {
		t.Errorf("error %v, want one that names the way out", err)
	}
}

func TestACustomUrlStillGoesThroughLoad(t *testing.T) {
	path := menu(t)
	before := slurp(t, path)
	if _, err := sh(t, path, "add", "mine", "sk-mine", "-url", "notaurl"); err == nil {
		t.Fatal("a url the gateway would refuse was installed")
	}
	if slurp(t, path) != before {
		t.Error("the rejected edit was installed anyway")
	}
}

func TestBareConfigGrowsTheSectionsItNeeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omny.yaml")
	body := "token: t0ken\nstate: " + filepath.Join(dir, "s.json") + "\npid:\ndefault: mine\nproviders:\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sh(t, path, "add", "mine", "sk-mine",
		"-url", "https://api.example.invalid/v1", "-model", "llama-4-scout"); err != nil {
		t.Fatal(err)
	}
	c, err := omny.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, slurp(t, path))
	}
	if p := c.Providers["mine"]; p == nil || len(p.Keys) != 1 {
		t.Errorf("provider %+v", p)
	}
	if got := c.Aliases["mine"]; len(got) != 1 || got[0] != "mine/llama-4-scout" {
		t.Errorf("alias mine is %v", got)
	}
}

func TestACustomProviderShowsUpArmed(t *testing.T) {
	path := menu(t)
	if _, err := sh(t, path, "add", "mine", "sk-secret", "-url", "https://api.example.invalid/v1"); err != nil {
		t.Fatal(err)
	}
	out, err := sh(t, path, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line(out, "mine", ""), "armed") {
		t.Errorf("ls says %q", line(out, "mine", ""))
	}
	if strings.Contains(out, "sk-secret") {
		t.Error("ls printed the key")
	}
}

func tailOf(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(l[max(0, len(l)-n):], "\n")
}

func TestKeylessArmsWithoutAKey(t *testing.T) {
	path := menu(t)
	before := slurp(t, path)

	out, err := sh(t, path, "add", "lmstudio", "-keyless", "-model", "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "lmstudio: keyless") {
		t.Errorf("add said %q, want it to say keyless rather than a key count", strings.TrimSpace(out))
	}

	c, err := omny.Load(path)
	if err != nil {
		t.Fatalf("the edited config does not load: %v", err)
	}
	p := c.Providers["lmstudio"]
	switch {
	case p == nil:
		t.Fatal("lmstudio is not in the catalogue")
	case !p.Keyless:
		t.Error("the provider was not armed")
	case len(p.Keys) != 0:
		t.Errorf("keys %v, want none", p.Keys)
	}
	if got := c.Aliases["lmstudio"]; len(got) != 1 || got[0] != "lmstudio/qwen3-8b" {
		t.Errorf("alias lmstudio is %v", got)
	}
	if n, m := len(strings.Split(before, "\n")), len(strings.Split(slurp(t, path), "\n")); m != n+2 {
		t.Errorf("the file went from %d lines to %d, want %d", n, m, n+2)
	}
}

func TestKeylessTakesNoKey(t *testing.T) {
	path := menu(t)
	before := slurp(t, path)
	if _, err := pipe(t, "sk-piped\n", path, "add", "lmstudio", "sk-typed", "-keyless"); err == nil {
		t.Fatal("a key was accepted alongside -keyless")
	}
	if slurp(t, path) != before {
		t.Error("the rejected edit was installed anyway")
	}
	if _, err := pipe(t, "sk-piped\n", path, "add", "lmstudio", "-keyless"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(slurp(t, path), "sk-piped") {
		t.Error("stdin was read for a keyless provider")
	}
}

func TestKeylessOnAKeyedProviderIsRefused(t *testing.T) {
	path := menu(t)
	if _, err := sh(t, path, "add", "groq", "sk-one"); err != nil {
		t.Fatal(err)
	}
	before := slurp(t, path)
	if _, err := sh(t, path, "add", "groq", "-keyless"); err == nil {
		t.Fatal("a provider was armed both ways at once")
	}
	if slurp(t, path) != before {
		t.Error("the rejected edit was installed anyway")
	}
}

func TestKeylessTwiceIsOnce(t *testing.T) {
	path := menu(t)
	if _, err := sh(t, path, "add", "vllm", "-keyless"); err != nil {
		t.Fatal(err)
	}
	once := slurp(t, path)
	if _, err := sh(t, path, "add", "vllm", "-keyless"); err != nil {
		t.Fatal(err)
	}
	if slurp(t, path) != once {
		t.Error("the second -keyless changed the file")
	}
}

func TestKeylessCreatesAProvider(t *testing.T) {
	path := menu(t)
	if _, err := sh(t, path, "add", "box", "-url", "http://127.0.0.1:9999/v1", "-keyless", "-model", "m"); err != nil {
		t.Fatal(err)
	}
	c, err := omny.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers["box"]
	if p == nil || !p.Keyless || p.URL != "http://127.0.0.1:9999/v1" {
		t.Errorf("provider %+v", p)
	}
}

func TestTheLocalServersAreOnTheMenu(t *testing.T) {
	cases := []struct {
		name string
		url  string
		key  bool
	}{
		{name: "lmstudio", url: "http://localhost:1234/v1"},
		{name: "ollama", url: "http://localhost:11434/v1"},
		{name: "vllm", url: "http://localhost:8000/v1"},
		{name: "litellm", url: "http://localhost:4000/v1", key: true},
	}
	base, err := omny.Parse(menu(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base.Providers[c.name]
			switch {
			case p == nil:
				t.Fatalf("%s is not in the catalogue", c.name)
			case p.URL != c.url:
				t.Errorf("url %q, want %q", p.URL, c.url)
			case p.Keyless:
				t.Error("it ships armed; the catalogue is a menu, not a manifest")
			case len(p.Keys) != 0:
				t.Error("it ships with a key")
			case p.TTFT == 0:
				t.Error("no ttft, and a cold local model takes minutes to its first token")
			}

			path, args := menu(t), []string{"add", c.name, "-keyless"}
			if c.key {
				args = []string{"add", c.name, "sk-master"}
			}
			if _, err := sh(t, path, args...); err != nil {
				t.Fatal(err)
			}
			got, err := omny.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if q := got.Providers[c.name]; q.Keyless == c.key && len(q.Keys) == 0 {
				t.Errorf("%s did not arm: %+v", c.name, q)
			}
		})
	}
	if base.Providers["ollamacloud"] == nil {
		t.Error("ollama.com lost its entry to the local daemon")
	}
}
