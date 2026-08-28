package edit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bharathajjarapu/omny/internal/omny"
)

type tweak func(lines []string, doc *yaml.Node) ([]string, error)

func amend(w io.Writer, path, prov string, tweaks ...tweak) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	// Reparse after each tweak because an insertion shifts later YAML line numbers.
	for _, t := range tweaks {
		var doc yaml.Node
		if err := yaml.Unmarshal(src, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		lines, err := t(strings.Split(string(src), "\n"), &doc)
		if err != nil {
			return err
		}
		src = []byte(strings.Join(lines, "\n"))
	}
	var fresh *omny.Config
	// Validate the temporary file with the server loader before replacing the real file.
	if err := omny.Replace(path, src, func(tmp string) error {
		c, err := omny.Load(tmp)
		if err != nil {
			return errors.New(strings.ReplaceAll(err.Error(), tmp, path))
		}
		fresh = c
		return nil
	}); err != nil {
		return err
	}
	armed := fmt.Sprintf("%d key(s)", len(fresh.Providers[prov].Keys))
	if fresh.Providers[prov].Keyless {
		armed = "keyless"
	}
	fmt.Fprintf(w, "%s: %s\n%s\n", prov, armed, nudge(fresh))
	return nil
}

func keys(prov string, f func([]string) ([]string, error)) tweak {
	return func(lines []string, doc *yaml.Node) ([]string, error) {
		at := provider(doc, prov)
		if at == nil {
			return nil, fmt.Errorf("no provider %q: omny ls lists them, and -url adds one", prov)
		}
		next, err := f(values(at, "keys"))
		if err != nil {
			return nil, err
		}
		return splice(lines, at, "provider "+strconv.Quote(prov), "keys", flow(next))
	}
}

func birth(prov, url string) tweak {
	return func(lines []string, doc *yaml.Node) ([]string, error) {
		if at := provider(doc, prov); at != nil {
			// Reject a different URL because changing it would silently move existing keys.
			if _, u := field(at, "url"); u != nil && u.Value != url {
				return nil, fmt.Errorf("provider %q already points at %s; edit the block by hand to move it", prov, u.Value)
			}
			return lines, nil
		}
		kn, ps := field(root(doc), "providers")
		if kn == nil {
			return nil, errors.New("config has no providers section")
		}
		last, col := edge(ps)
		if col == 0 {
			last, col = kn.Line, kn.Column+2
		}
		if last < 1 || last > len(lines) {
			return nil, fmt.Errorf("cannot locate the providers section in %d lines", len(lines))
		}
		pad := strings.Repeat(" ", col-1)
		return slices.Insert(slices.Clone(lines), last, pad+prov+":", pad+"  url: "+url), nil
	}
}

func bare(prov string) tweak {
	return func(lines []string, doc *yaml.Node) ([]string, error) {
		at := provider(doc, prov)
		if at == nil {
			return nil, fmt.Errorf("no provider %q: omny ls lists them, and -url adds one", prov)
		}
		return splice(lines, at, "provider "+strconv.Quote(prov), "keyless", "true")
	}
}

func pin(prov, model string) tweak {
	return func(lines []string, doc *yaml.Node) ([]string, error) {
		entry := prov + "/" + model
		an, as := field(root(doc), "aliases")
		if an == nil {
			out := slices.Clone(lines)
			for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			return append(out, "aliases:", render(3, prov, flow([]string{entry})), ""), nil
		}
		have := values(as, prov)
		if slices.Contains(have, entry) {
			return lines, nil
		}
		return splice(lines, as, "the aliases section", prov, flow(append(have, entry)))
	}
}

func splice(lines []string, at *yaml.Node, owner, name, val string) ([]string, error) {
	// Refuse flow mappings because a line edit could overwrite neighboring fields.
	if at.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("%s is written on one line; rewrite it as an indented block to edit it here", owner)
	}
	kn, kv := field(at, name)
	if kn == nil {
		last, col := edge(at)
		if last < 1 || last > len(lines) {
			return nil, fmt.Errorf("cannot locate %s in %d lines", owner, len(lines))
		}
		return slices.Insert(slices.Clone(lines), last, render(col, name, val)), nil
	}
	lo, hi := kn.Line, max(kn.Line, tail(kv))
	if hi > len(lines) {
		return nil, fmt.Errorf("%s entry at line %d is outside a %d line file", name, lo, len(lines))
	}
	// Preserve comments because the config is edited as text rather than re-encoded.
	note := kn.LineComment
	if note == "" && kv != nil {
		note = kv.LineComment
	}
	line := align(render(kn.Column, name, val), note, strings.Index(lines[lo-1], note))
	return slices.Concat(lines[:lo-1], []string{line}, lines[hi:]), nil
}

func render(col int, name, val string) string {
	return strings.Repeat(" ", col-1) + name + ": " + val
}

func flow(vals []string) string {
	q := make([]string, len(vals))
	for i, v := range vals {
		q[i] = strconv.Quote(v)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// Keep trailing comments so line edits do not discard operator notes.
func align(line, note string, col int) string {
	if note == "" {
		return line
	}
	if pad := col - len(line); pad > 0 {
		return line + strings.Repeat(" ", pad) + note
	}
	return line + "  " + note
}

func edge(m *yaml.Node) (line, col int) {
	if m == nil || len(m.Content) == 0 {
		return 0, 0
	}
	return tail(m), m.Content[0].Column
}

func tail(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	last := n.Line
	for _, c := range n.Content {
		last = max(last, tail(c))
	}
	return last
}

func root(doc *yaml.Node) *yaml.Node {
	if doc == nil || len(doc.Content) == 0 {
		return nil
	}
	return doc.Content[0]
}

func provider(doc *yaml.Node, name string) *yaml.Node {
	_, ps := field(root(doc), "providers")
	_, at := field(ps, name)
	return at
}

func field(m *yaml.Node, name string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == name {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

func values(m *yaml.Node, name string) []string {
	_, v := field(m, name)
	if v == nil {
		return nil
	}
	out := make([]string, 0, len(v.Content))
	for _, it := range v.Content {
		out = append(out, it.Value)
	}
	return out
}
