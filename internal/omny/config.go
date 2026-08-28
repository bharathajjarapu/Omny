package omny

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string               `yaml:"listen"`
	Token     string               `yaml:"token"`
	Tokens    map[string]string    `yaml:"tokens"`
	State     string               `yaml:"state"`
	Pid       string               `yaml:"pid"`
	Default   string               `yaml:"default"`
	Fallback  []string             `yaml:"fallback"`
	Providers map[string]*Provider `yaml:"providers"`
	Aliases   map[string][]string  `yaml:"aliases"`
}

type Provider struct {
	URL     string            `yaml:"url"`
	Keyless bool              `yaml:"keyless"`
	Headers map[string]string `yaml:"headers"`
	Keys    []string          `yaml:"keys"`

	RPD  int           `yaml:"rpd"`
	RPM  int           `yaml:"rpm"`
	TTFT time.Duration `yaml:"ttft"`
}

func (p *Provider) join(path string) string { return strings.TrimSuffix(p.URL, "/") + path }

func (p *Provider) endpoint() string { return p.join("/chat/completions") }
func (p *Provider) catalog() string  { return p.join("/models") }

// Allow provider declarations to raise the first-token budget, never lower it.
func (p *Provider) budget(d time.Duration) time.Duration {
	if p.TTFT > d {
		return p.TTFT
	}
	return d
}

// Omit Authorization for keyless providers instead of sending an empty bearer token.
func (p *Provider) dress(h http.Header, key string) {
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	for k, v := range p.Headers {
		h.Set(k, v)
	}
}

func Parse(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	// Check the opened file so the mode matches the bytes being parsed.
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	// Unix mode checks do not apply on Windows, where ACLs control access.
	if m := st.Mode().Perm(); runtime.GOOS != "windows" && m&0o077 != 0 {
		return nil, fmt.Errorf("config %s is mode %04o, want 0600 — it holds every key", path, m)
	}

	c := Config{Listen: "127.0.0.1:8080", State: "./omny.state.json", Pid: "./omny.pid"}
	dec := yaml.NewDecoder(f)
	// Reject unknown fields so configuration typos fail at startup.
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

func Load(path string) (*Config, error) {
	c, err := Parse(path)
	if err != nil {
		return nil, err
	}
	// Keep startup and config edits on the same validation path.
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) check() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	armed := 0
	twice := make(map[string]string)

	if c.Token == "" && len(c.Tokens) == 0 {
		add("no token: set token: or tokens:, because every request needs one")
	}
	for n, t := range c.Tokens {
		// Reject empty tokens because an empty bearer value must not authenticate.
		if t == "" {
			add("token %q is empty", n)
		}
	}
	if host, _, err := net.SplitHostPort(c.Listen); err != nil {
		add("listen %q is not host:port", c.Listen)
	} else if host == "" || host == "0.0.0.0" || host == "::" {
		add("listen %q binds every interface, want a loopback or Tailscale address", c.Listen)
	}
	if c.State == "" {
		add("state path is empty, and the daily counters need somewhere to live")
	}

	if len(c.Providers) == 0 {
		add("no providers configured")
	}
	for _, n := range slices.Sorted(maps.Keys(c.Providers)) {
		p := c.Providers[n]
		u, err := url.Parse(p.URL)
		switch {
		case p.URL == "":
			add("provider %s has no url", n)
		case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
			add("provider %s url %q is not an http(s) address", n, p.URL)
		}
		if p.Keyless && len(p.Keys) > 0 {
			add("provider %s is keyless and also has %d keys, want one or the other", n, len(p.Keys))
		}
		for i, k := range p.Keys {
			// Keep one secret in one pool slot so its quota state cannot split.
			switch was, dup := twice[k]; {
			case strings.TrimSpace(k) == "":
				add("provider %s key %d is empty", n, i)
			case dup:
				add("provider %s key %d is already %s's, and one key is one pool slot", n, i, was)
			default:
				twice[k] = n
			}
		}
		if p.RPD < 0 {
			add("provider %s rpd %d is negative, want 0 for unknown", n, p.RPD)
		}
		if p.RPM < 0 {
			add("provider %s rpm %d is negative, want 0 for unknown", n, p.RPM)
		}
		if p.TTFT < 0 {
			add("provider %s ttft %s is negative, want 0 for the default", n, p.TTFT)
		}
		if len(p.Keys) > 0 || p.Keyless {
			armed++
		}
	}
	if len(c.Providers) > 0 && armed == 0 {
		add("no provider has a key, so nothing can be routed anywhere")
	}

	for _, a := range slices.Sorted(maps.Keys(c.Aliases)) {
		if len(c.Aliases[a]) == 0 {
			add("alias %s is empty", a)
		}
		for _, e := range c.Aliases[a] {
			prov, model, ok := strings.Cut(e, "/")
			switch {
			case !ok || model == "" || prov == "":
				add("alias %s entry %q is not provider/model", a, e)
			case c.Providers[prov] == nil:
				add("alias %s names provider %q, which is not configured", a, prov)
			}
		}
	}

	switch {
	case c.Default == "":
		add("default is empty, so unrecognised model names have nowhere to go")
	case c.Providers[c.Default] == nil:
		add("default names provider %q, which is not configured", c.Default)
	}
	for _, a := range c.Fallback {
		if c.Aliases[a] == nil {
			add("fallback names alias %q, which is not configured", a)
		}
	}
	return errors.Join(errs...)
}

func (c *Config) who(tok string) string {
	name := ""
	// Check every candidate so timing does not reveal which token matched.
	if c.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(c.Token)) == 1 {
		name = "-"
	}
	for n, t := range c.Tokens {
		if t != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 {
			name = n
		}
	}
	return name
}
