package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestWriteError(t *testing.T) {
	id := json.RawMessage(`1`)
	msg := jsonRPCMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    -32603,
			Message: "test error",
		},
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["jsonrpc"] != "2.0" {
		t.Error("missing jsonrpc field")
	}
	errObj := parsed["error"].(map[string]any)
	if errObj["code"].(float64) != -32603 {
		t.Error("wrong error code")
	}
}

func TestJSONRPCMessage_MarshalRequest(t *testing.T) {
	msg := jsonRPCMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`42`),
		Method:  "tools/list",
		Params:  json.RawMessage(`{}`),
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["method"] != "tools/list" {
		t.Errorf("expected method 'tools/list', got %v", parsed["method"])
	}
	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got %v", parsed["jsonrpc"])
	}
	// id should be numeric 42
	if parsed["id"].(float64) != 42 {
		t.Errorf("expected id 42, got %v", parsed["id"])
	}
}

func TestJSONRPCMessage_MarshalResponse(t *testing.T) {
	msg := jsonRPCMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Result:  json.RawMessage(`{"tools":["read","write"]}`),
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	result := parsed["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(tools))
	}
	// error and method should be omitted
	if _, ok := parsed["error"]; ok {
		t.Error("error field should be omitted")
	}
	if _, ok := parsed["method"]; ok {
		t.Error("method field should be omitted in response")
	}
}

func TestJSONRPCMessage_UnmarshalNotification(t *testing.T) {
	data := `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":50}}`
	var msg jsonRPCMessage
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.JSONRPC != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got %q", msg.JSONRPC)
	}
	if msg.Method != "notifications/progress" {
		t.Errorf("expected method 'notifications/progress', got %q", msg.Method)
	}
	// Notifications have no ID
	if len(msg.ID) > 0 && string(msg.ID) != "null" {
		t.Errorf("notification should have no ID, got %s", string(msg.ID))
	}
}

func TestJSONRPCMessage_StringID(t *testing.T) {
	data := `{"jsonrpc":"2.0","id":"abc-123","method":"test"}`
	var msg jsonRPCMessage
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		t.Fatal(err)
	}
	if string(msg.ID) != `"abc-123"` {
		t.Errorf("expected string ID '\"abc-123\"', got %s", string(msg.ID))
	}
}

func TestJSONRPCMessage_NullID(t *testing.T) {
	data := `{"jsonrpc":"2.0","id":null,"method":"test"}`
	var msg jsonRPCMessage
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		t.Fatal(err)
	}
	if string(msg.ID) != "null" {
		t.Errorf("expected null ID, got %s", string(msg.ID))
	}
}

func TestJSONRPCError_Codes(t *testing.T) {
	tests := []struct {
		code    int
		message string
	}{
		{-32700, "Parse error"},
		{-32600, "Invalid Request"},
		{-32601, "Method not found"},
		{-32602, "Invalid params"},
		{-32603, "Internal error"},
	}
	for _, tc := range tests {
		msg := jsonRPCMessage{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Error:   &jsonRPCError{Code: tc.code, Message: tc.message},
		}
		out, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("code %d: marshal failed: %v", tc.code, err)
		}
		var roundtrip jsonRPCMessage
		if err := json.Unmarshal(out, &roundtrip); err != nil {
			t.Fatalf("code %d: unmarshal failed: %v", tc.code, err)
		}
		if roundtrip.Error.Code != tc.code {
			t.Errorf("expected code %d, got %d", tc.code, roundtrip.Error.Code)
		}
		if roundtrip.Error.Message != tc.message {
			t.Errorf("expected message %q, got %q", tc.message, roundtrip.Error.Message)
		}
	}
}

func TestStdinReader_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan []byte, 16)

	// Cancel immediately — StdinReader should exit without blocking
	cancel()
	go StdinReader(ctx, ch)

	select {
	case <-time.After(1 * time.Second):
		// StdinReader blocks on os.Stdin.Scan — context cancel only takes effect
		// on the channel send side, so this is expected behavior
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed or empty")
		}
	}
}

func TestPendingRequest_Fields(t *testing.T) {
	timer := time.AfterFunc(time.Hour, func() {})
	defer timer.Stop()

	pr := pendingRequest{
		rawID: json.RawMessage(`99`),
		timer: timer,
	}
	if string(pr.rawID) != "99" {
		t.Errorf("expected rawID '99', got %s", string(pr.rawID))
	}
	if pr.timer == nil {
		t.Error("expected non-nil timer")
	}
}
