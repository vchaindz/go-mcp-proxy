package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RunStreamableHTTP drives the Streamable HTTP transport loop.
func RunStreamableHTTP(ctx context.Context, tc *TransportConfig, firstLine []byte, firstResp *http.Response, stdinCh <-chan []byte) {
	var stdoutMu sync.Mutex
	responseCh := make(chan []byte, 64)

	// Extract session ID from the initialize response
	var sessionMu sync.Mutex
	sessionID := firstResp.Header.Get("Mcp-Session-Id")
	if sessionID != "" {
		log.Printf("session ID: %s", sessionID)
	}

	getSessionID := func() string {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		return sessionID
	}
	setSessionID := func(id string) {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if sessionID == "" && id != "" {
			sessionID = id
			log.Printf("session ID: %s", id)
		}
	}

	// Process the initialize response
	processHTTPResponse(firstResp, responseCh)

	// Start the SSE listener for server-initiated notifications
	go streamableSSEListener(ctx, tc, getSessionID, responseCh)

	log.Println("proxy ready (Streamable HTTP)")

	client := NewHTTPClient(tc.Insecure, 0) // no timeout; individual requests manage their own

	for {
		select {
		case data := <-responseCh:
			stdoutMu.Lock()
			writeStdout(data)
			stdoutMu.Unlock()

		case line, ok := <-stdinCh:
			if !ok {
				log.Println("stdin closed, shutting down")
				return
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}

			var msg jsonRPCMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				log.Printf("malformed JSON on stdin: %v", err)
				continue
			}

			hasID := len(msg.ID) > 0 && string(msg.ID) != "null"

			// POST in a goroutine so we don't block reading stdin
			go func(line []byte, msgID json.RawMessage, hasID bool) {
				resp, err := PostMessage(ctx, client, tc.ServerURL, getSessionID(), tc.Headers, line)
				if err != nil {
					log.Printf("POST failed: %v", err)
					if hasID {
						stdoutMu.Lock()
						writeError(msgID, -32603, fmt.Sprintf("POST failed: %v", err))
						stdoutMu.Unlock()
					}
					return
				}

				// Capture session ID if we don't have one yet
				if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
					setSessionID(sid)
				}

				if resp.StatusCode == 202 {
					// Accepted — no response body (notification from client)
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					return
				}

				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					log.Printf("POST returned status %d", resp.StatusCode)
					if hasID {
						stdoutMu.Lock()
						writeError(msgID, -32603, fmt.Sprintf("POST returned status %d", resp.StatusCode))
						stdoutMu.Unlock()
					}
					return
				}

				// Process the response (JSON or SSE)
				processHTTPResponse(resp, responseCh)
			}(line, msg.ID, hasID)

		case <-ctx.Done():
			return
		}
	}
}

// processHTTPResponse reads the HTTP response body and sends JSON-RPC messages to responseCh.
// Handles both application/json and text/event-stream content types.
func processHTTPResponse(resp *http.Response, responseCh chan<- []byte) {
	ct := resp.Header.Get("Content-Type")

	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		// SSE stream in response body — parse and forward each message event
		parseSSE(resp.Body, func(event, data string) {
			if event == "message" || event == "" {
				responseCh <- []byte(data)
			}
		})
		resp.Body.Close()

	case strings.HasPrefix(ct, "application/json"):
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("failed to read response body: %v", err)
			return
		}
		body = bytes.TrimSpace(body)
		if len(body) > 0 {
			responseCh <- body
		}

	default:
		// Unknown content type — try to read as JSON
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("failed to read response body: %v", err)
			return
		}
		body = bytes.TrimSpace(body)
		if len(body) > 0 && body[0] == '{' {
			responseCh <- body
		} else if len(body) > 0 {
			log.Printf("unexpected response content-type %q, body: %s", ct, string(body[:min(len(body), 200)]))
		}
	}
}

// streamableSSEListener opens a GET SSE connection for server-initiated notifications.
func streamableSSEListener(ctx context.Context, tc *TransportConfig, getSessionID func() string, responseCh chan<- []byte) {
	// Small delay to let the session establish
	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		return
	}

	sid := getSessionID()
	if sid == "" {
		log.Println("no session ID yet, skipping SSE listener")
		return
	}

	req, err := http.NewRequestWithContext(ctx, "GET", tc.ServerURL, nil)
	if err != nil {
		log.Printf("SSE listener: invalid URL: %v", err)
		return
	}
	applyHeaders(req, tc.Headers)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Mcp-Session-Id", sid)

	client := NewHTTPClient(tc.Insecure, 0)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("SSE listener: connection failed: %v", err)
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("SSE listener: got status %d (server may not support GET SSE)", resp.StatusCode)
		return
	}

	log.Println("SSE notification listener connected")

	err = parseSSE(resp.Body, func(event, data string) {
		if event == "message" || event == "" {
			responseCh <- []byte(data)
		}
	})

	if err != nil && ctx.Err() == nil {
		log.Printf("SSE listener: stream error: %v", err)
	}
}

// PostMessage sends a JSON-RPC message via HTTP POST.
func PostMessage(ctx context.Context, client *http.Client, serverURL, sessionID string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", serverURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyHeaders(req, headers)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	return client.Do(req)
}
