package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

func RunLegacySSE(ctx context.Context, tc *TransportConfig, firstLine []byte, stdinCh <-chan []byte) {
	var (
		mu      sync.Mutex
		pending = make(map[string]*pendingRequest)
	)

	sessionCh := make(chan string, 1)
	responseCh := make(chan []byte, 64)
	timeoutCh := make(chan json.RawMessage, 16)
	errCh := make(chan error, 1)

	go legacySSEReader(ctx, tc, sessionCh, responseCh, errCh)

	// Wait for session URL with 10s timeout
	var sessionURL string
	select {
	case sessionURL = <-sessionCh:
		log.Printf("session established: %s", sessionURL)
	case err := <-errCh:
		log.Fatalf("SSE connection failed: %v", err)
	case <-time.After(10 * time.Second):
		log.Fatal("timeout waiting for endpoint event (10s)")
	case <-ctx.Done():
		return
	}

	log.Println("proxy ready (legacy SSE)")

	client := NewHTTPClient(tc.Insecure, 30*time.Second)
	var stdoutMu sync.Mutex

	// Process the first line that was already read
	handleLegacyStdinLine(ctx, firstLine, sessionURL, client, tc.Headers, &stdoutMu, timeoutCh, &mu, pending)

	for {
		select {
		case line, ok := <-stdinCh:
			if !ok {
				log.Println("stdin closed, shutting down")
				failAllPending(&stdoutMu, &mu, pending)
				return
			}
			handleLegacyStdinLine(ctx, line, sessionURL, client, tc.Headers, &stdoutMu, timeoutCh, &mu, pending)

		case data := <-responseCh:
			handleLegacySSEResponse(data, &stdoutMu, &mu, pending)

		case rawID := <-timeoutCh:
			stdoutMu.Lock()
			writeError(rawID, -32603, "response timeout (30s)")
			stdoutMu.Unlock()

		case err := <-errCh:
			log.Printf("SSE stream error: %v", err)
			failAllPending(&stdoutMu, &mu, pending)
			return

		case <-ctx.Done():
			failAllPending(&stdoutMu, &mu, pending)
			return
		}
	}
}

// legacySSEReader connects to the SSE endpoint and dispatches events.
func legacySSEReader(ctx context.Context, tc *TransportConfig, sessionCh chan<- string, responseCh chan<- []byte, errCh chan<- error) {
	req, err := http.NewRequestWithContext(ctx, "GET", tc.ServerURL, nil)
	if err != nil {
		errCh <- fmt.Errorf("invalid SSE URL: %w", err)
		return
	}
	applyHeaders(req, tc.Headers)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	client := NewHTTPClient(tc.Insecure, 0)
	resp, err := client.Do(req)
	if err != nil {
		errCh <- fmt.Errorf("SSE connection failed: %w", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errCh <- fmt.Errorf("SSE returned status %d", resp.StatusCode)
		return
	}

	log.Printf("SSE connected to %s", tc.ServerURL)

	sessionSent := false
	err = parseSSE(resp.Body, func(event, data string) {
		switch event {
		case "endpoint":
			if sessionSent {
				return
			}
			resolved, err := resolveSessionURL(tc.ServerURL, data)
			if err != nil {
				log.Printf("invalid endpoint URL %q: %v", data, err)
				return
			}
			sessionCh <- resolved
			sessionSent = true
		case "message":
			responseCh <- []byte(data)
		}
	})

	if err != nil && ctx.Err() == nil {
		errCh <- fmt.Errorf("SSE stream error: %w", err)
	}
}

func handleLegacyStdinLine(ctx context.Context, line []byte, sessionURL string, client *http.Client, headers map[string]string, stdoutMu *sync.Mutex, timeoutCh chan<- json.RawMessage, mu *sync.Mutex, pending map[string]*pendingRequest) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}

	var msg jsonRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		log.Printf("malformed JSON on stdin: %v", err)
		return
	}

	hasID := len(msg.ID) > 0 && string(msg.ID) != "null"

	if hasID {
		idKey := string(msg.ID)
		timer := time.AfterFunc(30*time.Second, func() {
			mu.Lock()
			p, ok := pending[idKey]
			if ok {
				delete(pending, idKey)
			}
			mu.Unlock()
			if ok {
				timeoutCh <- p.rawID
			}
		})
		mu.Lock()
		pending[idKey] = &pendingRequest{rawID: msg.ID, timer: timer}
		mu.Unlock()
	}

	req, err := http.NewRequestWithContext(ctx, "POST", sessionURL, bytes.NewReader(line))
	if err != nil {
		log.Printf("failed to create POST request: %v", err)
		if hasID {
			removePending(string(msg.ID), mu, pending)
			stdoutMu.Lock()
			writeError(msg.ID, -32603, fmt.Sprintf("request error: %v", err))
			stdoutMu.Unlock()
		}
		return
	}
	applyHeaders(req, headers)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("POST failed: %v", err)
		if hasID {
			removePending(string(msg.ID), mu, pending)
			stdoutMu.Lock()
			writeError(msg.ID, -32603, fmt.Sprintf("POST failed: %v", err))
			stdoutMu.Unlock()
		}
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("POST returned status %d", resp.StatusCode)
		if hasID {
			removePending(string(msg.ID), mu, pending)
			stdoutMu.Lock()
			writeError(msg.ID, -32603, fmt.Sprintf("POST returned status %d", resp.StatusCode))
			stdoutMu.Unlock()
		}
	}
}

func handleLegacySSEResponse(data []byte, stdoutMu *sync.Mutex, mu *sync.Mutex, pending map[string]*pendingRequest) {
	var peek struct {
		ID json.RawMessage `json:"id,omitempty"`
	}
	json.Unmarshal(data, &peek)

	hasID := len(peek.ID) > 0 && string(peek.ID) != "null"

	if hasID {
		idKey := string(peek.ID)
		mu.Lock()
		p, ok := pending[idKey]
		if ok {
			p.timer.Stop()
			delete(pending, idKey)
		}
		mu.Unlock()
	}

	stdoutMu.Lock()
	writeStdout(data)
	stdoutMu.Unlock()
}

// removePending removes and stops a pending request by id key.
func removePending(idKey string, mu *sync.Mutex, pending map[string]*pendingRequest) {
	mu.Lock()
	if p, ok := pending[idKey]; ok {
		p.timer.Stop()
		delete(pending, idKey)
	}
	mu.Unlock()
}

// failAllPending fails all pending requests with an error.
func failAllPending(stdoutMu *sync.Mutex, mu *sync.Mutex, pending map[string]*pendingRequest) {
	mu.Lock()
	for idKey, p := range pending {
		p.timer.Stop()
		delete(pending, idKey)
		stdoutMu.Lock()
		writeError(p.rawID, -32603, "proxy shutting down")
		stdoutMu.Unlock()
	}
	mu.Unlock()
}
