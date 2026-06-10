package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxWireBody caps how many bytes of a request/response body the wire trace
// prints per message. The caller still receives the full body; only the trace
// output is truncated. Full JSON-RPC frames are available via -v, so the wire
// trace favours readability over completeness.
const maxWireBody = 4096

var (
	wireMu  sync.Mutex
	wireOut io.Writer // nil = wire tracing disabled
	wireSeq atomic.Uint64
)

// SetWireTrace enables HTTP wire tracing to w, or disables it when w is nil.
// Every client built by NewHTTPClient routes through the tracer, so enabling
// it covers CLI subcommands, the REPL, and stdio proxy mode alike.
func SetWireTrace(w io.Writer) {
	wireMu.Lock()
	wireOut = w
	wireMu.Unlock()
}

func wireEnabled() bool {
	wireMu.Lock()
	defer wireMu.Unlock()
	return wireOut != nil
}

func wirePrintf(format string, args ...any) {
	wireMu.Lock()
	defer wireMu.Unlock()
	if wireOut == nil {
		return
	}
	fmt.Fprintf(wireOut, time.Now().Format("15:04:05.000")+" wire "+format+"\n", args...)
}

// sensitiveHeaders are header names whose values must never appear in full in
// trace output. Lowercase for case-insensitive matching.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"api-key":             true,
	"apikey":              true,
	"x-auth-token":        true,
	"cookie":              true,
	"set-cookie":          true,
}

// RedactHeaderValue keeps a short prefix and the total length of sensitive
// header values so two systems' credentials can be compared (same token?
// same length?) without the trace ever containing a usable secret.
// Non-sensitive header values pass through unchanged.
func RedactHeaderValue(key, val string) string {
	if !sensitiveHeaders[strings.ToLower(key)] {
		return val
	}
	const keep = 10
	if len(val) <= keep {
		return fmt.Sprintf("***(redacted, %d chars)", len(val))
	}
	return fmt.Sprintf("%s…(redacted, %d chars total)", val[:keep], len(val))
}

func truncForWire(b []byte) string {
	if len(b) <= maxWireBody {
		return string(b)
	}
	return fmt.Sprintf("%s… (truncated, %d bytes total)", b[:maxWireBody], len(b))
}

// wireTracer is an http.RoundTripper that logs every HTTP exchange to the
// wire trace writer: request line, headers (secrets redacted), body,
// connection establishment, response status, headers, and body. SSE response
// bodies are logged line-by-line as they stream so a hanging connection shows
// exactly what arrived before the stall.
type wireTracer struct {
	base http.RoundTripper
}

func (t *wireTracer) RoundTrip(req *http.Request) (*http.Response, error) {
	if !wireEnabled() {
		return t.base.RoundTrip(req)
	}

	id := wireSeq.Add(1)
	wirePrintf("#%d → %s %s", id, req.Method, req.URL)
	logWireHeaders(id, "→", req.Header)

	// GetBody (set automatically for bytes.Reader bodies) yields a fresh copy
	// so logging never consumes the body the transport is about to send.
	if req.GetBody != nil {
		if rc, err := req.GetBody(); err == nil {
			b, _ := io.ReadAll(io.LimitReader(rc, maxWireBody*4))
			rc.Close()
			wirePrintf("#%d → body: %s", id, truncForWire(b))
		}
	}

	trace := &httptrace.ClientTrace{
		GotConn: func(ci httptrace.GotConnInfo) {
			wirePrintf("#%d ⇄ connected to %s (reused=%v)", id, ci.Conn.RemoteAddr(), ci.Reused)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	elapsed := time.Since(start)
	if err != nil {
		wirePrintf("#%d ← transport error after %s: %v", id, elapsed.Round(time.Millisecond), err)
		return resp, err
	}

	wirePrintf("#%d ← %s in %.1fms", id, resp.Status, float64(elapsed.Microseconds())/1000.0)
	logWireHeaders(id, "←", resp.Header)
	if resp.TLS != nil {
		wirePrintf("#%d ⇄ TLS %s, server cert CN=%q", id, tlsVersionName(resp.TLS.Version), tlsPeerCN(resp))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		// Stream: log each line as the consumer reads it.
		resp.Body = &sseWireBody{rc: resp.Body, id: id}
		return resp, nil
	}

	// Complete response: read it all, log it, and hand the caller a replay.
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		wirePrintf("#%d ← body read error: %v (got %d bytes)", id, rerr, len(body))
	}
	wirePrintf("#%d ← body: %s", id, truncForWire(body))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func logWireHeaders(id uint64, dir string, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			wirePrintf("#%d %s   %s: %s", id, dir, k, RedactHeaderValue(k, v))
		}
	}
}

func tlsVersionName(v uint16) string {
	switch v {
	case 0x0301:
		return "1.0"
	case 0x0302:
		return "1.1"
	case 0x0303:
		return "1.2"
	case 0x0304:
		return "1.3"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

func tlsPeerCN(resp *http.Response) string {
	if len(resp.TLS.PeerCertificates) == 0 {
		return ""
	}
	return resp.TLS.PeerCertificates[0].Subject.CommonName
}

// sseWireBody wraps a streaming response body and logs each complete line as
// the consumer reads it, so SSE traffic appears in the trace in real time.
type sseWireBody struct {
	rc  io.ReadCloser
	id  uint64
	buf bytes.Buffer
}

func (b *sseWireBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.buf.Write(p[:n])
		b.flushLines()
	}
	if err != nil {
		b.flushRemainder()
	}
	return n, err
}

func (b *sseWireBody) Close() error {
	b.flushRemainder()
	return b.rc.Close()
}

func (b *sseWireBody) flushLines() {
	for {
		i := bytes.IndexByte(b.buf.Bytes(), '\n')
		if i < 0 {
			return
		}
		line := make([]byte, i+1)
		b.buf.Read(line)
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			wirePrintf("#%d ← sse: %s", b.id, truncForWire(line))
		}
	}
}

func (b *sseWireBody) flushRemainder() {
	if b.buf.Len() == 0 {
		return
	}
	wirePrintf("#%d ← sse: %s", b.id, truncForWire(b.buf.Bytes()))
	b.buf.Reset()
}
