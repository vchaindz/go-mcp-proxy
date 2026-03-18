package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"time"
)

// JSON-RPC 2.0 types

type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Pending request tracking (used by legacy SSE transport)

type pendingRequest struct {
	rawID json.RawMessage
	timer *time.Timer
}

// StdinReader reads lines from stdin and sends them to the channel.
func StdinReader(ctx context.Context, ch chan<- []byte) {
	defer close(ch)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := make([]byte, len(scanner.Bytes()))
		copy(line, scanner.Bytes())
		select {
		case ch <- line:
		case <-ctx.Done():
			return
		}
	}
}

// writeStdout writes a JSON-RPC message line to stdout.
// Caller must hold the stdout mutex.
func writeStdout(data []byte) {
	os.Stdout.Write(data)
	os.Stdout.Write([]byte("\n"))
}

// writeError writes a JSON-RPC error response to stdout.
// Caller must hold the stdout mutex.
func writeError(id json.RawMessage, code int, message string) {
	resp := jsonRPCMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
		},
	}
	out, _ := json.Marshal(resp)
	os.Stdout.Write(out)
	os.Stdout.Write([]byte("\n"))
}
