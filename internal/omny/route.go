package omny

import "strings"

type target struct {
	prov  string
	model string
	at    *Provider
}

func (c *Config) route(name string) []target {
	if name == "" {
		return nil
	}
	if a, ok := c.Aliases[name]; ok {
		ts := make([]target, 0, len(a))
		for _, e := range a {
			prov, model, _ := strings.Cut(e, "/")
			ts = append(ts, target{prov, model, c.Providers[prov]})
		}
		return ts
	}
	// Split once because provider model IDs may contain slashes.
	if prov, model, ok := strings.Cut(name, "/"); ok && c.Providers[prov] != nil {
		return []target{{prov, model, c.Providers[prov]}}
	}
	return []target{{c.Default, name, c.Providers[c.Default]}}
}
