package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// syncBuffer is a goroutine-safe bytes.Buffer; the wire tracer writes from
// transport goroutines while tests read the accumulated output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRedactHeaderValue(t *testing.T) {
	tests := []struct {
		key, val string
		want     string // exact match for non-sensitive, substring for sensitive
		redacted bool
	}{
		{"Content-Type", "application/json", "application/json", false},
		{"Mcp-Session-Id", "sess-abc-123", "sess-abc-123", false},
		{"Authorization", "Bearer secret-token-value-123456", "", true},
		{"authorization", "Bearer secret-token-value-123456", "", true},
		{"X-Api-Key", "supersecretapikey9999", "", true},
		{"Cookie", "session=verysecretcookievalue", "", true},
	}
	for _, tt := range tests {
		got := RedactHeaderValue(tt.key, tt.val)
		if !tt.redacted {
			if got != tt.want {
				t.Errorf("RedactHeaderValue(%q, %q) = %q, want %q", tt.key, tt.val, got, tt.want)
			}
			continue
		}
		if strings.Contains(got, tt.val) {
			t.Errorf("RedactHeaderValue(%q) leaked full value: %q", tt.key, got)
		}
		// A short prefix must survive so two systems' tokens can be compared.
		if !strings.Contains(got, tt.val[:6]) {
			t.Errorf("RedactHeaderValue(%q) = %q, want prefix %q preserved", tt.key, got, tt.val[:6])
		}
		// The original length must be reported.
		if !strings.Contains(got, fmt.Sprintf("%d", len(tt.val))) {
			t.Errorf("RedactHeaderValue(%q) = %q, want length %d reported", tt.key, got, len(tt.val))
		}
	}
}

func TestWireTrace_JSONExchange(t *testing.T) {
	var buf syncBuffer
	SetWireTrace(&buf)
	defer SetWireTrace(nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "sess-from-server")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer srv.Close()

	client := NewHTTPClient(false, 0)
	resp, err := PostMessage(context.Background(), client, srv.URL, "sess-out",
		map[string]string{"Authorization": "Bearer secret-token-value-123456"},
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("response body altered by tracer: %s", body)
	}

	out := buf.String()
	for _, want := range []string{
		"POST " + srv.URL,                  // request line
		`"method":"ping"`,                  // request body
		"200",                              // response status
		`"ok":true`,                        // response body
		"Mcp-Session-Id: sess-out",         // request header
		"Mcp-Session-Id: sess-from-server", // response header
		"Authorization: Bearer",            // auth header present (prefix)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wire trace missing %q\n--- trace ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret-token-value-123456") {
		t.Errorf("wire trace leaked full auth token\n--- trace ---\n%s", out)
	}
}

func TestWireTrace_SSEBodyStreamsLines(t *testing.T) {
	var buf syncBuffer
	SetWireTrace(&buf)
	defer SetWireTrace(nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
	}))
	defer srv.Close()

	client := NewHTTPClient(false, 0)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "event: message") {
		t.Fatalf("SSE body altered by tracer: %s", body)
	}

	out := buf.String()
	if !strings.Contains(out, "event: message") {
		t.Errorf("wire trace missing SSE event line\n--- trace ---\n%s", out)
	}
	if !strings.Contains(out, `data: {"jsonrpc"`) {
		t.Errorf("wire trace missing SSE data line\n--- trace ---\n%s", out)
	}
}

func TestWireTrace_DisabledProducesNoOutputAndPassesThrough(t *testing.T) {
	SetWireTrace(nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	client := NewHTTPClient(false, 0)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"ok":true}` {
		t.Fatalf("body altered with tracing disabled: %s", body)
	}
}

func TestWireTrace_TruncatesLargeBodies(t *testing.T) {
	var buf syncBuffer
	SetWireTrace(&buf)
	defer SetWireTrace(nil)

	big := strings.Repeat("x", maxWireBody*2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"big":"%s"}`, big)
	}))
	defer srv.Close()

	client := NewHTTPClient(false, 0)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body) != len(big)+len(`{"big":""}`) {
		t.Fatalf("body truncated for the caller: got %d bytes", len(body))
	}

	out := buf.String()
	if !strings.Contains(out, "truncated") {
		t.Errorf("wire trace should note truncation for large bodies\n--- trace len %d ---", len(out))
	}
}
