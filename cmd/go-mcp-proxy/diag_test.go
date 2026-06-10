package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-mcp-proxy/internal/proxy"
)

// newMockMCPServer returns a Streamable HTTP MCP server that answers
// initialize, tools/list, ping, and tools/call. callErr, when non-empty,
// makes tools/call fail with a -32603 server error.
func newMockMCPServer(t *testing.T, callErr string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("mock server: bad request body: %v", err)
			w.WriteHeader(400)
			return
		}
		if len(msg.ID) == 0 { // notification
			w.WriteHeader(202)
			return
		}

		var result, rpcErr string
		switch msg.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"mock-prtg","version":"9.9"}}`
		case "tools/list":
			result = `{"tools":[{"name":"prtg_get_sensors","description":"d"},{"name":"prtg_get_sensor_status","description":"d"}]}`
		case "ping":
			result = `{}`
		case "tools/call":
			if callErr != "" {
				rpcErr = fmt.Sprintf(`{"code":-32603,"message":%q}`, callErr)
			} else {
				result = `{"content":[{"type":"text","text":"sensor is Up"}]}`
			}
		default:
			rpcErr = `{"code":-32601,"message":"method not found"}`
		}

		var body string
		if rpcErr != "" {
			body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":%s}`, msg.ID, rpcErr)
		} else {
			body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, msg.ID, result)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "mock-session-77")
		fmt.Fprint(w, body)
	}))
}

func TestCmdDiag_AllStepsPass(t *testing.T) {
	srv := newMockMCPServer(t, "")
	defer srv.Close()
	defer proxy.SetWireTrace(nil)

	out := filepath.Join(t.TempDir(), "report.txt")
	headers := proxy.HeaderFlag{"Authorization": "Bearer secret-token-value-123456"}

	err := cmdDiag([]string{srv.URL, "prtg_get_sensor_status", `{"sensor_id":65635}`, "-o", out}, "auto", false, headers)
	if err != nil {
		t.Fatalf("cmdDiag: %v", err)
	}

	data, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("read report: %v", rerr)
	}
	report := string(data)

	for _, want := range []string{
		"go-mcp-proxy diagnostic report", // header
		srv.URL,                          // target
		"initialize",                     // step section
		"tools/list",                     // step section
		"ping",                           // step section
		"prtg_get_sensor_status",         // tool call step
		"mock-prtg",                      // server info from initialize
		"mock-session-77",                // session ID
		"wire #",                         // wire trace included
		"Authorization: Bearer sec",      // redacted header prefix
		"summary",                        // summary section
		"OK",                             // step results
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q", want)
		}
	}
	if strings.Contains(report, "secret-token-value-123456") {
		t.Error("report leaked full auth token")
	}
	if strings.Contains(report, "FAIL") {
		t.Errorf("expected no FAIL in report:\n%s", report)
	}
}

func TestCmdDiag_ToolCallFails(t *testing.T) {
	srv := newMockMCPServer(t, "failed to get sensor: sensor not found")
	defer srv.Close()
	defer proxy.SetWireTrace(nil)

	out := filepath.Join(t.TempDir(), "report.txt")
	err := cmdDiag([]string{srv.URL, "prtg_get_sensor_status", `{"sensor_id":65635}`, "-o", out}, "auto", false, nil)
	if err == nil {
		t.Fatal("expected error when a step fails")
	}

	data, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("read report: %v", rerr)
	}
	report := string(data)
	if !strings.Contains(report, "FAIL") {
		t.Error("report should mark the failed step")
	}
	if !strings.Contains(report, "sensor not found") {
		t.Error("report should include the server error message")
	}
	// Earlier steps still succeeded and must be in the report.
	if !strings.Contains(report, "tools/list") {
		t.Error("report should still contain the tools/list step")
	}
}

func TestCmdDiag_NoToolJustConnectivity(t *testing.T) {
	srv := newMockMCPServer(t, "")
	defer srv.Close()
	defer proxy.SetWireTrace(nil)

	out := filepath.Join(t.TempDir(), "report.txt")
	if err := cmdDiag([]string{srv.URL, "-o", out}, "auto", false, nil); err != nil {
		t.Fatalf("cmdDiag: %v", err)
	}
	report, _ := os.ReadFile(out)
	if !strings.Contains(string(report), "tools/list") {
		t.Error("connectivity-only report should still list tools")
	}
	if strings.Contains(string(report), "tools/call") {
		t.Error("no tool was given; report should not contain a tools/call step")
	}
}

func TestCmdDiag_ConnectFailure(t *testing.T) {
	defer proxy.SetWireTrace(nil)
	out := filepath.Join(t.TempDir(), "report.txt")
	// Closed port: connection refused. The report must still be written.
	err := cmdDiag([]string{"http://127.0.0.1:1/mcp", "-o", out}, "http", false, nil)
	if err == nil {
		t.Fatal("expected error on unreachable server")
	}
	report, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("report not written on connect failure: %v", rerr)
	}
	if !strings.Contains(string(report), "FAIL") {
		t.Error("report should mark connect as failed")
	}
}
