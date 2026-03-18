package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestStreamableHTTP_RoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			// SSE notification listener — just hang until closed
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flush", 500)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher.Flush()
			<-r.Context().Done()
			return
		}

		// POST — return JSON response
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()

		var req jsonRPCMessage
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", 400)
			return
		}

		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":["test"]}}`, string(req.ID))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session-123")
		w.WriteHeader(200)
		w.Write([]byte(resp))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	initMsg := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := PostMessage(ctx, client, server.URL+"/mcp", "", nil, []byte(initMsg))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	sid := resp.Header.Get("Mcp-Session-Id")
	if sid != "test-session-123" {
		t.Errorf("expected session ID 'test-session-123', got %q", sid)
	}

	responseCh := make(chan []byte, 10)
	processHTTPResponse(resp, responseCh)

	select {
	case data := <-responseCh:
		var rpcResp jsonRPCMessage
		if err := json.Unmarshal(data, &rpcResp); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}
		if string(rpcResp.ID) != "1" {
			t.Errorf("expected id 1, got %s", string(rpcResp.ID))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response")
	}
}

func TestStreamableHTTP_SSEResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(405)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}

		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var req jsonRPCMessage
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":50}}\n\n")
		flusher.Flush()

		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"done\":true}}\n\n", string(req.ID))
		flusher.Flush()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	initMsg := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := PostMessage(ctx, client, server.URL+"/mcp", "", nil, []byte(initMsg))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}

	responseCh := make(chan []byte, 10)
	processHTTPResponse(resp, responseCh)

	var messages [][]byte
	for i := range 2 {
		select {
		case data := <-responseCh:
			messages = append(messages, data)
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for message %d", i)
		}
	}

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	if !bytes.Contains(messages[0], []byte("notifications/progress")) {
		t.Errorf("first message should be progress notification, got: %s", string(messages[0]))
	}

	if !bytes.Contains(messages[1], []byte(`"done":true`)) {
		t.Errorf("second message should be result, got: %s", string(messages[1]))
	}
}

func TestStreamableHTTP_SessionIDPropagation(t *testing.T) {
	var receivedSessionIDs []string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(405)
			return
		}

		mu.Lock()
		receivedSessionIDs = append(receivedSessionIDs, r.Header.Get("Mcp-Session-Id"))
		mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var req jsonRPCMessage
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-abc")
		w.WriteHeader(200)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, string(req.ID))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Second}

	resp1, err := PostMessage(ctx, client, server.URL, "", nil, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	sid := resp1.Header.Get("Mcp-Session-Id")
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()

	resp2, err := PostMessage(ctx, client, server.URL, sid, nil, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(receivedSessionIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(receivedSessionIDs))
	}
	if receivedSessionIDs[0] != "" {
		t.Errorf("first request should have no session ID, got %q", receivedSessionIDs[0])
	}
	if receivedSessionIDs[1] != "session-abc" {
		t.Errorf("second request should have session ID 'session-abc', got %q", receivedSessionIDs[1])
	}
}

func TestStreamableHTTP_202Notification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(405)
			return
		}
		w.WriteHeader(202)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Second}
	notification := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`
	resp, err := PostMessage(ctx, client, server.URL, "", nil, []byte(notification))
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 202 {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestPostMessage_CustomHeaders(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	headers := map[string]string{
		"Authorization": "Bearer test-token",
		"X-Custom":      "hello",
	}
	resp, err := PostMessage(ctx, client, server.URL, "sess-123", headers, []byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if receivedHeaders.Get("Authorization") != "Bearer test-token" {
		t.Errorf("expected Authorization header, got %q", receivedHeaders.Get("Authorization"))
	}
	if receivedHeaders.Get("X-Custom") != "hello" {
		t.Errorf("expected X-Custom header, got %q", receivedHeaders.Get("X-Custom"))
	}
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type should be application/json, got %q", receivedHeaders.Get("Content-Type"))
	}
	if receivedHeaders.Get("Mcp-Session-Id") != "sess-123" {
		t.Errorf("Mcp-Session-Id should be sess-123, got %q", receivedHeaders.Get("Mcp-Session-Id"))
	}
}

func TestPostMessage_NilHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := PostMessage(ctx, client, server.URL, "", nil, []byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestPostMessage_NoSessionID(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := PostMessage(ctx, client, server.URL, "", nil, []byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if receivedHeaders.Get("Mcp-Session-Id") != "" {
		t.Errorf("should not send Mcp-Session-Id when empty, got %q", receivedHeaders.Get("Mcp-Session-Id"))
	}
}

func TestPostMessage_ProtocolHeadersOverrideCustom(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	// Try to override Content-Type and Accept via custom headers
	headers := map[string]string{
		"Content-Type": "text/plain",
		"Accept":       "text/html",
	}
	resp, err := PostMessage(ctx, client, server.URL, "sid", headers, []byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Protocol headers should take precedence
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type should be overridden to application/json, got %q", receivedHeaders.Get("Content-Type"))
	}
	if receivedHeaders.Get("Accept") != "application/json, text/event-stream" {
		t.Errorf("Accept should be overridden, got %q", receivedHeaders.Get("Accept"))
	}
}

func TestProcessHTTPResponse_UnknownContentType_JSON(t *testing.T) {
	// Server returns no Content-Type but JSON body
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	responseCh := make(chan []byte, 10)
	processHTTPResponse(resp, responseCh)

	select {
	case data := <-responseCh:
		if !bytes.Contains(data, []byte(`"result"`)) {
			t.Errorf("expected JSON result, got %s", string(data))
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestProcessHTTPResponse_UnknownContentType_NonJSON(t *testing.T) {
	// Server returns non-JSON body with no Content-Type
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		w.Write([]byte("this is not json"))
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	responseCh := make(chan []byte, 10)
	processHTTPResponse(resp, responseCh)

	select {
	case data := <-responseCh:
		t.Errorf("should not dispatch non-JSON data, got %s", string(data))
	case <-time.After(100 * time.Millisecond):
		// expected — nothing dispatched
	}
}

func TestProcessHTTPResponse_EmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// empty body
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	responseCh := make(chan []byte, 10)
	processHTTPResponse(resp, responseCh)

	select {
	case data := <-responseCh:
		t.Errorf("should not dispatch empty body, got %s", string(data))
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestStreamableSSEListener_CustomHeaders(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"test\"}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	tc := &TransportConfig{
		ServerURL: server.URL,
		Headers:   map[string]string{"Authorization": "Bearer sse-token"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	responseCh := make(chan []byte, 10)
	sid := "test-session"
	streamableSSEListener(ctx, tc, func() string { return sid }, responseCh)

	if receivedHeaders.Get("Authorization") != "Bearer sse-token" {
		t.Errorf("expected Authorization header on SSE GET, got %q", receivedHeaders.Get("Authorization"))
	}
	if receivedHeaders.Get("Mcp-Session-Id") != "test-session" {
		t.Errorf("expected Mcp-Session-Id on SSE GET, got %q", receivedHeaders.Get("Mcp-Session-Id"))
	}
}

func TestStreamableSSEListener_NoSessionID(t *testing.T) {
	// Should return early without connecting when session ID is empty
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not connect when no session ID")
		w.WriteHeader(500)
	}))
	defer server.Close()

	tc := &TransportConfig{ServerURL: server.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	responseCh := make(chan []byte, 10)
	streamableSSEListener(ctx, tc, func() string { return "" }, responseCh)

	select {
	case data := <-responseCh:
		t.Errorf("should not receive data, got %s", string(data))
	default:
		// expected
	}
}

func TestStreamableSSEListener_ServerRejectsGET(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(405)
	}))
	defer server.Close()

	tc := &TransportConfig{ServerURL: server.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	responseCh := make(chan []byte, 10)
	streamableSSEListener(ctx, tc, func() string { return "sid" }, responseCh)

	select {
	case data := <-responseCh:
		t.Errorf("should not receive data on 405, got %s", string(data))
	default:
		// expected
	}
}
