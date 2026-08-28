package edit

import (
	"bytes"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathajjarapu/omny/internal/omny"
)

const catalogue = "../../omny.example.yaml"

func menu(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(catalogue)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := strings.Replace(string(src), "token:  ", "token: t0ken  ", 1)
	out = strings.Replace(out, "state: /var/lib/omny/omny.state.json",
		"state: "+filepath.Join(dir, "s.json"), 1)
	out = strings.Replace(out, "pid: /run/omny/omny.pid", "pid:", 1)
	path := filepath.Join(dir, "omny.yaml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := omny.Load(path); err != nil {
		t.Fatalf("the fixture does not load: %v", err)
	}
	return path
}

func sh(t *testing.T, path string, args ...string) (string, error) {
	t.Helper()
	return pipe(t, "", path, args...)
}

func pipe(t *testing.T, in, path string, args ...string) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := CLI(&b, strings.NewReader(in), path, args)
	return b.String(), err
}

func slurp(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEditPreservesTheCatalogue(t *testing.T) {
	path := menu(t)
	before := slurp(t, path)

	if _, err := sh(t, path, "add", "groq", "sk-one"); err != nil {
		t.Fatal(err)
	}
	added := slurp(t, path)
	if _, err := sh(t, path, "rm", "groq", "sk-one"); err != nil {
		t.Fatal(err)
	}
	after := slurp(t, path)

	if n, m := strings.Count(before, "#"), strings.Count(after, "#"); n != m {
		t.Errorf("%d comment markers became %d", n, m)
	}
	if before != after {
		t.Errorf("add then rm did not return the file to what it was")
	}
	diff := 0
	for i, l := range strings.Split(added, "\n") {
		if b := strings.Split(before, "\n"); l != b[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Errorf("adding one key changed %d lines, want 1", diff)
	}
}

func TestAddRefusesADuplicate(t *testing.T) {
	path := menu(t)
	if _, err := sh(t, path, "add", "groq", "sk-one"); err != nil {
		t.Fatal(err)
	}
	_, err := sh(t, path, "add", "groq", "sk-one")
	if err == nil {
		t.Fatal("the same key was added twice")
	}
	if !strings.Contains(err.Error(), "already") {
		t.Errorf("error %q does not say the key is already there", err)
	}
}

func TestAddRefusesAKeyAnotherProviderHas(t *testing.T) {
	path := menu(t)
	if _, err := sh(t, path, "add", "groq", "sk-shared"); err != nil {
		t.Fatal(err)
	}
	before := slurp(t, path)
	if _, err := sh(t, path, "add", "cerebras", "sk-shared"); err == nil {
		t.Fatal("a key was written into two providers")
	}
	if slurp(t, path) != before {
		t.Error("the rejected edit was installed anyway")
	}
}

func TestAddNamesAnUnknownProvider(t *testing.T) {
	path := menu(t)
	_, err := sh(t, path, "add", "nosuch", "sk-one")
	if err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error %v, want one naming the provider", err)
	}
}

func TestRemoveByHashPrefix(t *testing.T) {
	path := menu(t)
	sh(t, path, "add", "groq", "sk-one")
	sh(t, path, "add", "groq", "sk-two")

	id := omny.Fingerprint("sk-one")
	if _, err := sh(t, path, "rm", "groq", id[:4]); err != nil {
		t.Fatal(err)
	}
	got := slurp(t, path)
	if strings.Contains(got, "sk-one") || !strings.Contains(got, "sk-two") {
		t.Errorf("wrong key removed:\n%s", line(got, "keys:", "groq"))
	}
}

func TestRemoveWithoutAMatch(t *testing.T) {
	path := menu(t)
	sh(t, path, "add", "groq", "sk-one")
	if _, err := sh(t, path, "rm", "groq", "nothinglikethat"); err == nil {
		t.Error("removing a key that is not there succeeded")
	}
}

func TestABadResultIsNotInstalled(t *testing.T) {
	path := menu(t)
	before := slurp(t, path)
	if _, err := sh(t, path, "add", "ovh", "sk-one"); err == nil {
		t.Fatal("a key was added to a keyless provider")
	}
	if slurp(t, path) != before {
		t.Error("the file was changed by an edit that does not validate")
	}
}

func TestEditKeepsTheMode(t *testing.T) {
	path := menu(t)
	if _, err := sh(t, path, "add", "groq", "sk-one"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := st.Mode().Perm(); m != 0o600 {
		t.Errorf("mode %04o after an edit, want 0600", m)
	}
}

func TestKeysAreInsertedWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omny.yaml")
	os.WriteFile(path, []byte(`token: t0ken
state: `+filepath.Join(dir, "s.json")+`
pid:
default: solo
providers:
  solo:                 # a provider with no keys line at all
    url: https://example.invalid/v1
    keyless: true
`), 0o600)

	if _, err := omny.Load(path); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(strings.Replace(slurp(t, path), "    keyless: true\n", "", 1)+"  other:\n    url: https://x.invalid/v1\n    keyless: true\n"), 0o600)

	if _, err := sh(t, path, "add", "solo", "sk-one"); err != nil {
		t.Fatal(err)
	}
	got := slurp(t, path)
	if !strings.Contains(got, `keys: ["sk-one"]`) {
		t.Errorf("no keys line was inserted:\n%s", got)
	}
	if !strings.Contains(got, "# a provider with no keys line at all") {
		t.Errorf("the comment was lost:\n%s", got)
	}
	if _, err := omny.Load(path); err != nil {
		t.Errorf("the result does not load: %v", err)
	}
}

func TestListHidesKeyMaterial(t *testing.T) {
	path := menu(t)
	sh(t, path, "add", "groq", "sk-very-secret")
	out, err := sh(t, path, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "sk-very-secret") {
		t.Errorf("ls printed a key:\n%s", out)
	}
	if !strings.Contains(out, omny.Fingerprint("sk-very-secret")) {
		t.Errorf("ls did not print the hash prefix:\n%s", out)
	}
	state := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(l); len(f) > 1 {
			state[f[0]] = f[1]
		}
	}
	if state["ovh"] != "keyless" || state["cerebras"] != "off" || state["groq"] != "armed" {
		t.Errorf("ls reported ovh=%q cerebras=%q groq=%q", state["ovh"], state["cerebras"], state["groq"])
	}
}

func TestCheck(t *testing.T) {
	path := menu(t)
	if out, err := sh(t, path, "check"); err != nil || !strings.Contains(out, "ok") {
		t.Errorf("check said %q, %v on a good config", out, err)
	}
	os.WriteFile(path, []byte("token: t0ken\nproviders:\n"), 0o600)
	if _, err := sh(t, path, "check"); err == nil {
		t.Error("check accepted a config with no providers")
	}
}

func TestUnknownCommand(t *testing.T) {
	if _, err := sh(t, menu(t), "frobnicate"); err == nil {
		t.Error("an unknown command was accepted")
	}
}

func line(s string, a, b string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, a) && strings.Contains(l, b) {
			return l
		}
	}
	return s
}

func TestNudge(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	cases := []struct {
		name string
		pid  string
		want string
	}{
		{"no pidfile configured", "", "kill -HUP"},
		{"no pidfile on disk", filepath.Join(dir, "absent.pid"), "no gateway running"},
		{"a pidfile holding nonsense", write("junk.pid", "banana\n"), "does not hold a pid"},
		{"a pid nothing is running under", write("stale.pid", "2147483646\n"), "stale pidfile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nudge(&omny.Config{Pid: c.pid}); !strings.Contains(got, c.want) {
				t.Errorf("nudge said %q, want it to mention %q", got, c.want)
			}
		})
	}
}

func TestNudgeSignalsTheRunningProcess(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "omny.pid")
	os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	if got := nudge(&omny.Config{Pid: p}); !strings.Contains(got, "reloaded") {
		t.Fatalf("nudge said %q", got)
	}
	select {
	case <-hup:
	case <-time.After(2 * time.Second):
		t.Error("no SIGHUP arrived")
	}
}

func TestTheKeysLineCommentSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omny.yaml")
	os.WriteFile(path, []byte(`token: t0ken
state: `+filepath.Join(dir, "s.json")+`
pid:
default: solo
providers:
  solo:
    url: https://example.invalid/v1
    keys: []              # paste one here
`), 0o600)

	if _, err := sh(t, path, "add", "solo", "sk-one"); err != nil {
		t.Fatal(err)
	}
	got := slurp(t, path)
	if !strings.Contains(got, "# paste one here") {
		t.Errorf("the note beside keys was dropped:\n%s", got)
	}
	if !strings.Contains(got, `keys: ["sk-one"]`) {
		t.Errorf("the key was not written:\n%s", got)
	}
}

func TestFlowProviderIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omny.yaml")
	body := `token: t0ken
state: ` + filepath.Join(dir, "s.json") + `
pid:
default: solo
providers:
  solo: {url: "https://example.invalid/v1", keys: ["sk-old"]}
`
	os.WriteFile(path, []byte(body), 0o600)

	_, err := sh(t, path, "add", "solo", "sk-new")
	if err == nil {
		t.Fatal("a flow-style provider was edited")
	}
	if !strings.Contains(err.Error(), "one line") {
		t.Errorf("error %q does not say what is wrong", err)
	}
	if slurp(t, path) != body {
		t.Error("the file was changed anyway")
	}
}

func TestKeyFromStdin(t *testing.T) {
	cases := []struct {
		name string
		args []string
		in   string
		key  string
		bad  bool
	}{
		{name: "no key argument", args: []string{"add", "groq"}, in: "sk-piped\n", key: "sk-piped"},
		{name: "an explicit dash", args: []string{"add", "groq", "-"}, in: "sk-dashed\n", key: "sk-dashed"},
		{name: "surrounding space is not the key", args: []string{"add", "groq"}, in: "  sk-trimmed  \n", key: "sk-trimmed"},
		{name: "nothing on stdin", args: []string{"add", "groq"}, in: "", bad: true},
		{name: "a blank line is not a key", args: []string{"add", "groq"}, in: "   \n", bad: true},
		{name: "two keys at once", args: []string{"add", "groq", "sk-a", "sk-b"}, in: "sk-c\n", bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := menu(t)
			_, err := pipe(t, c.in, path, c.args...)
			if c.bad {
				if err == nil {
					t.Fatalf("accepted %v with stdin %q", c.args, c.in)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := slurp(t, path); !strings.Contains(got, `keys: ["`+c.key+`"]`) {
				t.Errorf("keys line is %q, want %s", line(got, "keys:", ""), c.key)
			}
		})
	}
}
