package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLegacySSE_RoundTrip(t *testing.T) {
	sseWriter := make(chan string, 10)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming not supported", 500)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(200)

			fmt.Fprintf(w, "event: endpoint\ndata: /session\n\n")
			flusher.Flush()

			for msg := range sseWriter {
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
				flusher.Flush()
			}

		case "POST":
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()

			var req jsonRPCMessage
			if err := json.Unmarshal(body, &req); err == nil && len(req.ID) > 0 {
				resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, string(req.ID))
				sseWriter <- resp
			}

			w.WriteHeader(202)
		}
	}))
	defer server.Close()
	defer close(sseWriter)

	tc := &TransportConfig{
		ServerURL: server.URL + "/sse",
		Headers:   map[string]string{},
	}

	sessionCh := make(chan string, 1)
	responseCh := make(chan []byte, 64)
	errCh := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go legacySSEReader(ctx, tc, sessionCh, responseCh, errCh)

	select {
	case sessionURL := <-sessionCh:
		if !strings.Contains(sessionURL, "/session") {
			t.Errorf("unexpected session URL: %s", sessionURL)
		}
	case err := <-errCh:
		t.Fatalf("SSE error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for session URL")
	}
}

func TestLegacySSE_CustomHeaders(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			receivedHeaders = r.Header.Clone()
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flush", 500)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fmt.Fprintf(w, "event: endpoint\ndata: /session\n\n")
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(202)
	}))
	defer server.Close()

	tc := &TransportConfig{
		ServerURL: server.URL + "/sse",
		Headers:   map[string]string{"Authorization": "Bearer secret"},
	}

	sessionCh := make(chan string, 1)
	responseCh := make(chan []byte, 64)
	errCh := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go legacySSEReader(ctx, tc, sessionCh, responseCh, errCh)

	select {
	case <-sessionCh:
		// success
	case err := <-errCh:
		t.Fatalf("SSE error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for session URL")
	}

	if receivedHeaders.Get("Authorization") != "Bearer secret" {
		t.Errorf("expected Authorization header on SSE GET, got %q", receivedHeaders.Get("Authorization"))
	}
}

func TestLegacySSEReader_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer server.Close()

	tc := &TransportConfig{
		ServerURL: server.URL,
		Headers:   map[string]string{},
	}

	sessionCh := make(chan string, 1)
	responseCh := make(chan []byte, 64)
	errCh := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go legacySSEReader(ctx, tc, sessionCh, responseCh, errCh)

	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("expected error mentioning 403, got %v", err)
		}
	case <-sessionCh:
		t.Fatal("should not get session URL on 403")
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

func TestLegacySSEReader_MessageEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		fmt.Fprintf(w, "event: endpoint\ndata: /session\n\n")
		flusher.Flush()

		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
		flusher.Flush()

		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notify\"}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	tc := &TransportConfig{
		ServerURL: server.URL,
		Headers:   map[string]string{},
	}

	sessionCh := make(chan string, 1)
	responseCh := make(chan []byte, 64)
	errCh := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go legacySSEReader(ctx, tc, sessionCh, responseCh, errCh)

	// Wait for session
	select {
	case <-sessionCh:
	case err := <-errCh:
		t.Fatalf("SSE error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	// Collect messages
	var messages []string
	for i := 0; i < 2; i++ {
		select {
		case data := <-responseCh:
			messages = append(messages, string(data))
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for message %d", i)
		}
	}

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if !strings.Contains(messages[0], `"id":1`) {
		t.Errorf("first message should contain id:1, got %s", messages[0])
	}
	if !strings.Contains(messages[1], `"notify"`) {
		t.Errorf("second message should contain notify, got %s", messages[1])
	}
}

func TestHandleLegacyStdinLine_EmptyLine(t *testing.T) {
	var stdoutMu sync.Mutex
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)
	timeoutCh := make(chan json.RawMessage, 16)

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	// Empty line should be a no-op
	handleLegacyStdinLine(ctx, []byte("  "), "http://localhost", client, nil, &stdoutMu, timeoutCh, &mu, pending)

	mu.Lock()
	if len(pending) != 0 {
		t.Errorf("expected no pending requests, got %d", len(pending))
	}
	mu.Unlock()
}

func TestHandleLegacyStdinLine_MalformedJSON(t *testing.T) {
	var stdoutMu sync.Mutex
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)
	timeoutCh := make(chan json.RawMessage, 16)

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	// Malformed JSON should be a no-op
	handleLegacyStdinLine(ctx, []byte("not json"), "http://localhost", client, nil, &stdoutMu, timeoutCh, &mu, pending)

	mu.Lock()
	if len(pending) != 0 {
		t.Errorf("expected no pending requests, got %d", len(pending))
	}
	mu.Unlock()
}

func TestHandleLegacyStdinLine_PostWithHeaders(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(202)
	}))
	defer server.Close()

	var stdoutMu sync.Mutex
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)
	timeoutCh := make(chan json.RawMessage, 16)

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	headers := map[string]string{"X-Test": "value123"}

	line := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	handleLegacyStdinLine(ctx, line, server.URL, client, headers, &stdoutMu, timeoutCh, &mu, pending)

	if receivedHeaders.Get("X-Test") != "value123" {
		t.Errorf("expected X-Test header, got %q", receivedHeaders.Get("X-Test"))
	}
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", receivedHeaders.Get("Content-Type"))
	}
}

func TestHandleLegacyStdinLine_TracksRequestWithID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
	}))
	defer server.Close()

	var stdoutMu sync.Mutex
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)
	timeoutCh := make(chan json.RawMessage, 16)

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	line := []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/list"}`)
	handleLegacyStdinLine(ctx, line, server.URL, client, nil, &stdoutMu, timeoutCh, &mu, pending)

	mu.Lock()
	p, ok := pending["42"]
	mu.Unlock()

	if !ok {
		t.Fatal("expected pending request for id 42")
	}
	if string(p.rawID) != "42" {
		t.Errorf("expected rawID '42', got %s", string(p.rawID))
	}
	p.timer.Stop()
}

func TestHandleLegacySSEResponse_ClearsPending(t *testing.T) {
	var stdoutMu sync.Mutex
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)

	timer := time.AfterFunc(time.Hour, func() {})
	mu.Lock()
	pending["1"] = &pendingRequest{rawID: json.RawMessage(`1`), timer: timer}
	mu.Unlock()

	data := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	handleLegacySSEResponse(data, &stdoutMu, &mu, pending)

	mu.Lock()
	_, exists := pending["1"]
	mu.Unlock()

	if exists {
		t.Error("pending request should be cleared after response")
	}
}

func TestHandleLegacySSEResponse_NotificationNoID(t *testing.T) {
	var stdoutMu sync.Mutex
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)

	// Should not panic when there's no ID
	data := []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
	handleLegacySSEResponse(data, &stdoutMu, &mu, pending)

	mu.Lock()
	if len(pending) != 0 {
		t.Errorf("pending should remain empty, got %d", len(pending))
	}
	mu.Unlock()
}

func TestRemovePending(t *testing.T) {
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)

	timer := time.AfterFunc(time.Hour, func() {})
	pending["99"] = &pendingRequest{rawID: json.RawMessage(`99`), timer: timer}

	removePending("99", &mu, pending)

	mu.Lock()
	_, exists := pending["99"]
	mu.Unlock()

	if exists {
		t.Error("pending request should be removed")
	}
}

func TestRemovePending_Nonexistent(t *testing.T) {
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)

	// Should not panic for nonexistent key
	removePending("does-not-exist", &mu, pending)
}

func TestFailAllPending(t *testing.T) {
	var stdoutMu sync.Mutex
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)

	for i := 1; i <= 3; i++ {
		timer := time.AfterFunc(time.Hour, func() {})
		idStr := fmt.Sprintf("%d", i)
		pending[idStr] = &pendingRequest{
			rawID: json.RawMessage(idStr),
			timer: timer,
		}
	}

	failAllPending(&stdoutMu, &mu, pending)

	mu.Lock()
	if len(pending) != 0 {
		t.Errorf("expected all pending cleared, got %d", len(pending))
	}
	mu.Unlock()
}

func TestFailAllPending_Empty(t *testing.T) {
	var stdoutMu sync.Mutex
	var mu sync.Mutex
	pending := make(map[string]*pendingRequest)

	// Should not panic with empty map
	failAllPending(&stdoutMu, &mu, pending)
}
