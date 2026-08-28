package omny

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reuse fixed buffers so streaming relay allocations stay bounded.
var buffers = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// Separate client write failures so upstream keys are not benched.
var errGone = errors.New("client went away")

// Keep the tail because streaming usage arrives in the final frame.
type sniff struct {
	buf [512]byte
	n   int
}

func (s *sniff) feed(p []byte) {
	if len(p) >= len(s.buf) {
		s.n = copy(s.buf[:], p[len(p)-len(s.buf):])
		return
	}
	if drop := s.n + len(p) - len(s.buf); drop > 0 {
		s.n = copy(s.buf[:], s.buf[drop:s.n])
	}
	s.n += copy(s.buf[s.n:], p)
}

// Use the last object-valued usage field because earlier frames may contain usage:null.
func (s *sniff) count() *spend {
	const mark = `"usage":`
	i := bytes.LastIndex(s.buf[:s.n], []byte(mark))
	if i < 0 {
		return nil
	}
	b := bytes.TrimLeft(s.buf[i+len(mark):s.n], " \t")
	if len(b) == 0 || b[0] != '{' {
		return nil
	}
	depth := 0
	for j := range b {
		switch b[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				var v spend
				if json.Unmarshal(b[:j+1], &v) != nil {
					return nil
				}
				v.fill()
				return &v
			}
		}
	}
	return nil
}

type result struct {
	res    *http.Response
	first  []byte
	rest   io.Reader
	stream bool
	ttft   time.Duration
	used   *spend
	saw    sniff
	stop   func() // stores cancellation so the relay and idle timer share ownership.
}

func (r *result) cost() *spend {
	if r.used == nil {
		r.used = r.saw.count()
	}
	return r.used
}

func relay(w http.ResponseWriter, out *result, idle time.Duration) error {
	defer out.stop()
	defer out.res.Body.Close()

	head(w.Header(), out.res.Header)
	if !out.stream {
		w.Header().Set("Content-Length", strconv.Itoa(len(out.first)))
	}
	w.WriteHeader(http.StatusOK)

	// Write the gated bytes before the rest so the first token stays in order.
	if _, err := w.Write(out.first); err != nil {
		return fmt.Errorf("%w: %w", errGone, err)
	}
	if !out.stream {
		return nil
	}

	rc := http.NewResponseController(w)
	_ = rc.Flush()

	// Reset this timer after every chunk because it measures idle gaps, not total time.
	quiet := time.AfterFunc(idle, out.stop)
	defer quiet.Stop()

	b := buffers.Get().(*[]byte)
	defer buffers.Put(b)
	return pump(w, out.rest, *b, rc, func(p []byte) {
		quiet.Reset(idle)
		out.saw.feed(p)
	})
}

// Use a manual loop because io.CopyBuffer can bypass the flush hook.
func pump(w io.Writer, src io.Reader, buf []byte, rc *http.ResponseController, each func([]byte)) error {
	for {
		n, err := src.Read(buf)
		if n > 0 {
			each(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				return fmt.Errorf("%w: %w", errGone, werr)
			}
			_ = rc.Flush()
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("read upstream: %w", err)
		}
	}
}

func head(dst, src http.Header) {
	if ct := src.Get("Content-Type"); ct != "" {
		dst.Set("Content-Type", ct)
	}
	for h, v := range src {
		if strings.HasPrefix(strings.ToLower(h), "x-ratelimit-") {
			dst[h] = slices.Clone(v)
		}
	}
}
