package proxy

import (
	"strings"
	"testing"
)

func TestParseSSE_BasicMessage(t *testing.T) {
	input := "data: hello world\n\n"
	var events []struct{ event, data string }

	err := parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].event != "message" {
		t.Errorf("expected default event type 'message', got %q", events[0].event)
	}
	if events[0].data != "hello world" {
		t.Errorf("expected data 'hello world', got %q", events[0].data)
	}
}

func TestParseSSE_NamedEvent(t *testing.T) {
	input := "event: endpoint\ndata: /session/abc\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].event != "endpoint" {
		t.Errorf("expected event 'endpoint', got %q", events[0].event)
	}
	if events[0].data != "/session/abc" {
		t.Errorf("expected data '/session/abc', got %q", events[0].data)
	}
}

func TestParseSSE_MultiLineData(t *testing.T) {
	input := "data: line1\ndata: line2\ndata: line3\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].data != "line1\nline2\nline3" {
		t.Errorf("expected multi-line data, got %q", events[0].data)
	}
}

func TestParseSSE_Comments(t *testing.T) {
	input := ": this is a comment\ndata: actual\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].data != "actual" {
		t.Errorf("expected 'actual', got %q", events[0].data)
	}
}

func TestParseSSE_MultipleEvents(t *testing.T) {
	input := "event: endpoint\ndata: /sess\n\nevent: message\ndata: {\"id\":1}\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].event != "endpoint" || events[0].data != "/sess" {
		t.Errorf("event 0: got %+v", events[0])
	}
	if events[1].event != "message" || events[1].data != `{"id":1}` {
		t.Errorf("event 1: got %+v", events[1])
	}
}

func TestParseSSE_EmptyData(t *testing.T) {
	input := "\n\ndata: hello\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

func TestParseSSE_NoLeadingSpace(t *testing.T) {
	input := "data:nospace\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].data != "nospace" {
		t.Errorf("expected 'nospace', got %q", events[0].data)
	}
}

func TestParseSSE_OnlyComments(t *testing.T) {
	input := ": comment 1\n: comment 2\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 0 {
		t.Errorf("expected 0 events for comment-only stream, got %d", len(events))
	}
}

func TestParseSSE_EventResetBetweenDispatches(t *testing.T) {
	// After dispatch, event type should reset to default
	input := "event: custom\ndata: first\n\ndata: second\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].event != "custom" {
		t.Errorf("first event should be 'custom', got %q", events[0].event)
	}
	if events[1].event != "message" {
		t.Errorf("second event should reset to 'message', got %q", events[1].event)
	}
}

func TestParseSSE_EmptyStream(t *testing.T) {
	var events []struct{ event, data string }
	err := parseSSE(strings.NewReader(""), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestParseSSE_UnknownFieldIgnored(t *testing.T) {
	input := "id: 123\ndata: hello\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].data != "hello" {
		t.Errorf("expected 'hello', got %q", events[0].data)
	}
}

func TestParseSSE_DataWithoutTerminalNewline(t *testing.T) {
	// Stream that ends without a trailing blank line — data should not be dispatched
	input := "data: incomplete"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 0 {
		t.Errorf("expected 0 events (no dispatch without blank line), got %d", len(events))
	}
}

func TestParseSSE_JSONData(t *testing.T) {
	jsonPayload := `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`
	input := "event: message\ndata: " + jsonPayload + "\n\n"
	var events []struct{ event, data string }

	parseSSE(strings.NewReader(input), func(event, data string) {
		events = append(events, struct{ event, data string }{event, data})
	})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].data != jsonPayload {
		t.Errorf("JSON payload mismatch: got %q", events[0].data)
	}
}

func TestResolveSessionURL_Absolute(t *testing.T) {
	result, err := resolveSessionURL("http://host:3001/sse", "http://host:3001/session/abc")
	if err != nil {
		t.Fatal(err)
	}
	if result != "http://host:3001/session/abc" {
		t.Errorf("got %q", result)
	}
}

func TestResolveSessionURL_Relative(t *testing.T) {
	result, err := resolveSessionURL("http://host:3001/sse", "/session/abc")
	if err != nil {
		t.Fatal(err)
	}
	if result != "http://host:3001/session/abc" {
		t.Errorf("got %q", result)
	}
}

func TestResolveSessionURL_RelativeNoSlash(t *testing.T) {
	result, err := resolveSessionURL("http://host:3001/mcp/sse", "session/abc")
	if err != nil {
		t.Fatal(err)
	}
	if result != "http://host:3001/mcp/session/abc" {
		t.Errorf("got %q", result)
	}
}

func TestResolveSessionURL_WhitespaceEndpoint(t *testing.T) {
	result, err := resolveSessionURL("http://host:3001/sse", "  /session/abc  ")
	if err != nil {
		t.Fatal(err)
	}
	if result != "http://host:3001/session/abc" {
		t.Errorf("got %q", result)
	}
}

func TestResolveSessionURL_WithQueryParams(t *testing.T) {
	result, err := resolveSessionURL("http://host:3001/sse", "/session/abc?token=xyz")
	if err != nil {
		t.Fatal(err)
	}
	if result != "http://host:3001/session/abc?token=xyz" {
		t.Errorf("got %q", result)
	}
}

func TestResolveSessionURL_DifferentPort(t *testing.T) {
	result, err := resolveSessionURL("http://host:3001/sse", "http://host:4000/session/abc")
	if err != nil {
		t.Fatal(err)
	}
	if result != "http://host:4000/session/abc" {
		t.Errorf("got %q", result)
	}
}

func TestResolveSessionURL_InvalidBase(t *testing.T) {
	_, err := resolveSessionURL("://invalid", "/session")
	if err == nil {
		t.Error("expected error for invalid base URL")
	}
}

func TestResolveSessionURL_EmptyEndpoint(t *testing.T) {
	result, err := resolveSessionURL("http://host:3001/sse", "")
	if err != nil {
		t.Fatal(err)
	}
	// Empty endpoint resolves to the base
	if result != "http://host:3001/sse" {
		t.Errorf("got %q", result)
	}
}
