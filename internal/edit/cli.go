package edit

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/bharathajjarapu/omny/internal/omny"
)

func Usage(w io.Writer) {
	fmt.Fprint(w, `usage: omny [-c config] [command]

with no command, runs the gateway.

  add <provider> [key]   add a key; with no key, or with "-", reads one from stdin
      -url <base>        create the provider first, at any OpenAI-compatible base URL
      -model <id>        also name that model, so "model": "<provider>" reaches it
      -keyless           arm it with no key at all, the way a local server wants
  rm  <provider> <key>   remove a key, by value or by the hash prefix /status prints
  ls                     list providers, whether each is armed, and its key prefixes
  check                  validate the config and exit non-zero if it will not load

add and rm signal the running gateway to reload; with none running the file is still
edited and the change applies at the next start.
`)
}

func CLI(w io.Writer, in io.Reader, path string, args []string) error {
	switch args[0] {
	case "check":
		if _, err := omny.Load(path); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: ok\n", path)
		return nil

	case "ls":
		return list(w, path)

	case "add":
		return plus(w, in, path, args[1:])

	case "rm":
		if len(args) != 3 {
			return fmt.Errorf("rm: want a provider name and a key or hash prefix")
		}
		return amend(w, path, args[1], keys(args[1], func(ks []string) ([]string, error) {
			return drop(ks, args[2])
		}))
	}
	Usage(w)
	return fmt.Errorf("unknown command %q", args[0])
}

func plus(w io.Writer, in io.Reader, path string, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(w)
	url := fs.String("url", "", "OpenAI-compatible base URL; creates the provider when it is new")
	model := fs.String("model", "", "reach this provider's model by the provider's own name")
	none := fs.Bool("keyless", false, "arm it with no Authorization header at all, the way a local server wants")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("add: want a provider name")
	}
	prov := rest[0]
	var work []tweak
	if *url != "" {
		// The gateway appends the completion path, so this flag takes a base URL.
		if strings.Contains(*url, "/chat/completions") {
			return fmt.Errorf("add: -url wants the base URL, not the endpoint; drop the /chat/completions")
		}
		work = append(work, birth(prov, *url))
	}
	if *none {
		// Keyless mode must not consume stdin for a key.
		if len(rest) > 1 {
			return fmt.Errorf("add: -keyless takes no key, got %q", rest[1])
		}
		work = append(work, bare(prov))
	} else {
		key, err := secret(in, rest[1:])
		if err != nil {
			return err
		}
		work = append(work, keys(prov, func(ks []string) ([]string, error) {
			return append(slices.Clone(ks), key), nil
		}))
	}
	if *model != "" {
		work = append(work, pin(prov, *model))
	}
	return amend(w, path, prov, work...)
}

// Parse one positional argument at a time because FlagSet stops at the first one.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}

// Read keys from stdin by default so they stay out of shell history.
func secret(in io.Reader, rest []string) (string, error) {
	if len(rest) > 1 {
		return "", fmt.Errorf("add: want one key, got %d arguments", len(rest))
	}
	if len(rest) == 1 && rest[0] != "-" {
		return rest[0], nil
	}
	s := bufio.NewScanner(in)
	if !s.Scan() {
		return "", fmt.Errorf("add: no key on stdin")
	}
	key := strings.TrimSpace(s.Text())
	if key == "" {
		return "", fmt.Errorf("add: the key on stdin is empty")
	}
	return key, nil
}

func drop(ks []string, want string) ([]string, error) {
	out, hit := make([]string, 0, len(ks)), 0
	for _, v := range ks {
		if v == want || strings.HasPrefix(omny.Fingerprint(v), want) {
			hit++
			continue
		}
		out = append(out, v)
	}
	switch {
	case hit == 0:
		return nil, fmt.Errorf("rm: no key matching %q", want)
	case hit > 1:
		return nil, fmt.Errorf("rm: %q matches %d keys; give more of the prefix", want, hit)
	}
	return out, nil
}

func list(w io.Writer, path string) error {
	// Use Parse so operators can inspect a file that is not valid yet.
	c, err := omny.Parse(path)
	if err != nil {
		return err
	}
	for _, n := range slices.Sorted(maps.Keys(c.Providers)) {
		p := c.Providers[n]
		ids := make([]string, 0, len(p.Keys))
		for _, k := range p.Keys {
			ids = append(ids, omny.Fingerprint(k))
		}
		switch {
		case p.Keyless:
			fmt.Fprintf(w, "%-14s keyless\n", n)
		case len(ids) == 0:
			fmt.Fprintf(w, "%-14s off\n", n)
		default:
			fmt.Fprintf(w, "%-14s armed  %s\n", n, strings.Join(ids, " "))
		}
	}
	return nil
}

func nudge(c *omny.Config) string {
	if c.Pid == "" {
		return "no pidfile configured; reload with kill -HUP yourself"
	}
	b, err := os.ReadFile(c.Pid)
	if err != nil {
		return "no gateway running; the change applies at next start"
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Sprintf("pidfile %s does not hold a pid; reload by hand", c.Pid)
	}
	// Check the pid before sending SIGHUP so a stale pidfile is not treated as live.
	p, err := os.FindProcess(pid)
	if err != nil || p.Signal(syscall.Signal(0)) != nil {
		return fmt.Sprintf("no process %d (stale pidfile %s); the change applies at next start", pid, c.Pid)
	}
	if err := p.Signal(syscall.SIGHUP); err != nil {
		return fmt.Sprintf("could not signal pid %d: %v", pid, err)
	}
	return fmt.Sprintf("reloaded pid %d", pid)
}
