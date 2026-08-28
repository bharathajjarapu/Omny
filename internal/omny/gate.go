package omny

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Bound buffered responses so a broken provider cannot consume unbounded memory.
const (
	maxFrame = 256 << 10
	maxReply = 64 << 20
)

// Wait for content instead of status because providers can put errors in a 200 SSE frame.
func gate(res *http.Response, stream bool) (*result, error) {
	if !stream {
		b, err := io.ReadAll(io.LimitReader(res.Body, maxReply+1))
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if len(b) > maxReply {
			return nil, fmt.Errorf("response exceeds %d bytes", maxReply)
		}
		if !json.Valid(b) {
			return nil, fmt.Errorf("answered 200 with %d bytes that are not JSON", len(b))
		}
		used, err := verdict(b)
		if err != nil {
			return nil, err
		}
		return &result{res: res, first: b, rest: http.NoBody, used: used}, nil
	}

	br := bufio.NewReaderSize(res.Body, 8<<10)
	var first, line []byte
	for {
		// Use ReadSlice so an unterminated SSE line cannot grow without a bound.
		chunk, err := br.ReadSlice('\n')
		first, line = append(first, chunk...), append(line, chunk...)
		if len(first) > maxFrame {
			return nil, fmt.Errorf("no content in the first %d bytes of the stream", maxFrame)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stream ended before any content: %w", err)
		}

		data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
		line = nil
		if !ok {
			continue
		}
		if data = bytes.TrimSpace(data); len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if _, err := verdict(data); err != nil {
			return nil, err
		}
		return &result{res: res, first: first, rest: br, stream: true}, nil
	}
}

type spend struct {
	In    int `json:"prompt_tokens"`
	Out   int `json:"completion_tokens"`
	Total int `json:"total_tokens"`
}

func (s spend) add(o spend) spend {
	return spend{s.In + o.In, s.Out + o.Out, s.Total + o.Total}
}

func verdict(b []byte) (*spend, error) {
	var v struct {
		Error *json.RawMessage `json:"error"`
		Usage *spend           `json:"usage"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, nil //nolint:nilerr
	}
	if v.Error != nil {
		return nil, fmt.Errorf("answered 200 carrying %s", *v.Error)
	}
	if v.Usage != nil {
		v.Usage.fill()
	}
	return v.Usage, nil
}

// Fill a missing total from both components so omitted totals are not counted as zero.
func (s *spend) fill() {
	if s.Total == 0 {
		s.Total = s.In + s.Out
	}
}

// Parse provider reset formats so cooldowns honor their retry hints.
func after(h http.Header, now time.Time) time.Duration {
	for _, name := range []string{"Retry-After", "X-RateLimit-Reset-Requests", "X-RateLimit-Reset"} {
		v := h.Get(name)
		if v == "" {
			continue
		}
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			if d := time.Duration(secs * float64(time.Second)); d > 0 {
				return d
			}
			continue
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := t.Sub(now); d > 0 {
				return d
			}
			continue
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 0
}
