package omny

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Buffer the body because failover attempts resend it.
const maxBody = 32 << 20

func slurp(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	src := http.MaxBytesReader(w, r.Body, maxBody)
	n := r.ContentLength
	if n <= 0 || n > maxBody {
		return io.ReadAll(src)
	}
	buf := bytes.NewBuffer(make([]byte, 0, n))
	if _, err := buf.ReadFrom(src); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type want struct {
	Model   string          `json:"model"`
	Stream  bool            `json:"stream"`
	Options json.RawMessage `json:"stream_options"`
}

func peek(b []byte) (want, error) {
	var v want
	if err := json.Unmarshal(b, &v); err != nil {
		return want{}, fmt.Errorf("parse request: %w", err)
	}
	return v, nil
}

const counted = `{"include_usage":true}`

const inject = `,"stream_options":` + counted

type plan struct {
	body []byte
	want
	lo, hi int
	end    int
}

func readBody(b []byte) (*plan, error) {
	v, err := peek(b)
	if err != nil {
		return nil, err
	}
	p := &plan{body: b, want: v}
	p.lo, p.hi, p.end = locate(b)
	return p, nil
}

func locate(b []byte) (lo, hi, end int) {
	end = len(b) - 1
	for end >= 0 && (b[end] == ' ' || b[end] == '\n' || b[end] == '\t' || b[end] == '\r') {
		end--
	}
	if end <= 0 || b[end] != '}' {
		return 0, 0, 0
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return 0, 0, 0
	}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return 0, 0, 0
		}
		if key, _ := k.(string); key != "model" {
			// Use Token rather than Decode so skipped values are not copied.
			if err := step(d); err != nil {
				return 0, 0, 0
			}
			continue
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return 0, 0, 0
		}
		at := int(d.InputOffset())
		// Verify decoder offsets before splicing so an unexpected shape falls back safely.
		if at < len(raw) || !bytes.Equal(b[at-len(raw):at], raw) {
			return 0, 0, 0
		}
		return at - len(raw), at, end
	}
	return 0, 0, 0
}

func step(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'), json.Delim('['):
	default:
		return nil
	}
	for depth := 1; depth > 0; {
		switch t, err := d.Token(); {
		case err != nil:
			return err
		case t == json.Delim('{') || t == json.Delim('['):
			depth++
		case t == json.Delim('}') || t == json.Delim(']'):
			depth--
		}
	}
	return nil
}

func (p *plan) send(model string, meter bool) (func() io.Reader, int64, error) {
	add := meter && p.Options == nil
	if model == p.Model && !add {
		return func() io.Reader { return bytes.NewReader(p.body) }, int64(len(p.body)), nil
	}
	if p.hi == 0 {
		b, err := swap(p.body, model, p.want, meter)
		if err != nil {
			return nil, 0, err
		}
		return func() io.Reader { return bytes.NewReader(b) }, int64(len(b)), nil
	}
	name, err := json.Marshal(model)
	if err != nil {
		return nil, 0, fmt.Errorf("encode model name: %w", err)
	}
	head, tail := p.body[:p.lo], p.body[p.hi:]
	n := int64(len(head) + len(name) + len(tail))
	if !add {
		return func() io.Reader {
			return io.MultiReader(bytes.NewReader(head), bytes.NewReader(name), bytes.NewReader(tail))
		}, n, nil
	}
	mid, last := p.body[p.hi:p.end], p.body[p.end:]
	n = int64(len(head) + len(name) + len(mid) + len(inject) + len(last))
	return func() io.Reader {
		return io.MultiReader(bytes.NewReader(head), bytes.NewReader(name), bytes.NewReader(mid),
			strings.NewReader(inject), bytes.NewReader(last))
	}, n, nil
}

func swap(b []byte, model string, asked want, meter bool) ([]byte, error) {
	if model == asked.Model && (!meter || asked.Options != nil) {
		return b, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	name, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("encode model name: %w", err)
	}
	// Keep unknown fields as RawMessage so the slow path does not rewrite them.
	m["model"] = name
	if _, mine := m["stream_options"]; meter && !mine {
		// Never overwrite a caller's stream_options field.
		m["stream_options"] = json.RawMessage(counted)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // APIs expect JSON characters without browser escaping.
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return buf.Bytes(), nil
}
