package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in       string
		wantCmd  string
		wantRest string
	}{
		{"tools", "tools", ""},
		{"  tools  ", "tools", ""},
		{"call prtg_get_sensors", "call", "prtg_get_sensors"},
		{`call prtg_get_sensors {"compact":true,"limit":1}`, "call", `prtg_get_sensors {"compact":true,"limit":1}`},
		{"raw\t{\"jsonrpc\":\"2.0\"}", "raw", `{"jsonrpc":"2.0"}`},
		{"", "", ""},
	}
	for _, c := range cases {
		gotCmd, gotRest := splitCommand(c.in)
		if gotCmd != c.wantCmd || gotRest != c.wantRest {
			t.Errorf("splitCommand(%q) = (%q, %q), want (%q, %q)", c.in, gotCmd, gotRest, c.wantCmd, c.wantRest)
		}
	}
}

// TestDebugClient_NoClientTimeout is the load-bearing test: it verifies the
// HTTP client built for debug sessions has Timeout: 0 so the operator can
// observe long-running or hung server responses without the client confounding
// the diagnosis.
func TestDebugClient_NoClientTimeout(t *testing.T) {
	tc := &TransportConfig{ServerURL: "http://example.invalid", Headers: map[string]string{}}
	dc := NewClient(context.Background(), tc)
	if dc.client.Timeout != 0 {
		t.Errorf("debug client must have Timeout=0, got %v", dc.client.Timeout)
	}
}

func TestDebugClient_DispatchRoutesByID(t *testing.T) {
	tc := &TransportConfig{ServerURL: "http://example.invalid", Headers: map[string]string{}}
	dc := NewClient(context.Background(), tc)

	respCh1 := make(chan json.RawMessage, 1)
	respCh2 := make(chan json.RawMessage, 1)
	dc.pending["1"] = respCh1
	dc.pending["2"] = respCh2

	dc.dispatch([]byte(`{"jsonrpc":"2.0","id":2,"result":{"a":2}}`))
	dc.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{"a":1}}`))
	dc.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"p":50}}`))

	select {
	case got := <-respCh1:
		if !contains(got, `"a":1`) {
			t.Errorf("ch1 got %s", string(got))
		}
	case <-time.After(time.Second):
		t.Fatal("ch1 timeout")
	}
	select {
	case got := <-respCh2:
		if !contains(got, `"a":2`) {
			t.Errorf("ch2 got %s", string(got))
		}
	case <-time.After(time.Second):
		t.Fatal("ch2 timeout")
	}
	select {
	case got := <-dc.notifCh:
		if !contains(got, "notifications/progress") {
			t.Errorf("notifCh got %s", string(got))
		}
	case <-time.After(time.Second):
		t.Fatal("notifCh timeout (notification should be routed there)")
	}
}

// TestDebugClient_AutoInitializeStreamableHTTP exercises the full auto-detect
// path against a mock Streamable HTTP server: probe POST → 200 + initialize
// reply → notifications/initialized → tools/list round-trip.
func TestDebugClient_AutoInitializeStreamableHTTP(t *testing.T) {
	var initialized atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			// Notification listener — keep open until cancelled.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var req jsonRPCMessage
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		// Notifications: 202 no body
		if len(req.ID) == 0 || string(req.ID) == "null" {
			if req.Method == "notifications/initialized" {
				initialized.Store(true)
			}
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "debug-test-session")
		w.WriteHeader(200)
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"mock"},"capabilities":{}}}`, string(req.ID))
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"prtg_get_sensors"}]}}`, string(req.ID))
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, string(req.ID))
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tc := &TransportConfig{
		ServerURL: server.URL + "/mcp",
		Headers:   map[string]string{},
		Type:      "auto",
	}
	dc := NewClient(ctx, tc)
	go dc.notificationDrain()

	if err := dc.autoInitialize(); err != nil {
		t.Fatalf("autoInitialize: %v", err)
	}
	if dc.transport != "http" {
		t.Errorf("expected transport=http, got %s", dc.transport)
	}
	dc.sessMu.Lock()
	gotSID := dc.sessionID
	dc.sessMu.Unlock()
	if gotSID != "debug-test-session" {
		t.Errorf("expected session ID, got %q", gotSID)
	}

	// Now exercise tools/list through the same client to confirm the
	// pending-map / dispatch loop works end-to-end.
	resp, err := dc.SendRequest("tools/list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if !contains(resp, "prtg_get_sensors") {
		t.Errorf("tools/list result missing tool, got %s", string(resp))
	}

	// Give the mock server a moment to record the notifications/initialized POST.
	time.Sleep(200 * time.Millisecond)
	if !initialized.Load() {
		t.Error("server never received notifications/initialized")
	}
}

// TestDebugClient_LegacySSE exercises the legacy SSE path: GET stream,
// endpoint event, POST initialize, response correlation via SSE.
func TestDebugClient_LegacySSE(t *testing.T) {
	// Events the SSE handler should emit. The handler owns the ResponseWriter
	// so all writes happen on a single goroutine.
	sseEvents := make(chan string, 8)
	postedBodies := make(chan []byte, 8)

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Send the endpoint event first.
		fmt.Fprintf(w, "event: endpoint\ndata: /messages?session=abc\n\n")
		f.Flush()
		// Then drain queued events until the client disconnects.
		for {
			select {
			case ev := <-sseEvents:
				fmt.Fprint(w, ev)
				f.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		postedBodies <- body
		w.WriteHeader(200)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tc := &TransportConfig{
		ServerURL: server.URL + "/sse",
		Headers:   map[string]string{},
		Type:      "sse",
	}
	dc := NewClient(ctx, tc)
	go dc.notificationDrain()

	if err := dc.connectLegacySSE(); err != nil {
		t.Fatalf("connectLegacySSE: %v", err)
	}

	// Watch for the POSTed initialize and queue the matching response.
	go func() {
		body := <-postedBodies
		var req jsonRPCMessage
		json.Unmarshal(body, &req)
		sseEvents <- fmt.Sprintf("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"ok\":true}}\n\n", string(req.ID))
	}()

	resp, err := dc.SendRequest("initialize", json.RawMessage(debugInitParams))
	if err != nil {
		t.Fatalf("sendRequest: %v", err)
	}
	if !contains(resp, `"ok":true`) {
		t.Errorf("expected ok:true, got %s", string(resp))
	}
}

func contains(b []byte, s string) bool {
	return bytesIndex(b, []byte(s)) >= 0
}

func TestLoadREPLJSONArgs_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/args.json"
	want := `{"sensor_id":30235}`
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadREPLJSONArgs("@" + path)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLoadREPLJSONArgs_MissingFile(t *testing.T) {
	_, err := loadREPLJSONArgs("@/nonexistent/path/that/should/not/exist.json")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadREPLJSONArgs_BareAt(t *testing.T) {
	_, err := loadREPLJSONArgs("@")
	if err == nil {
		t.Fatal("expected error for bare '@', got nil")
	}
}

// Avoid pulling in another import — small inline implementation.
func bytesIndex(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
