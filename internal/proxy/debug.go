package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const debugInitParams = `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"go-mcp-proxy-debug","version":"1.0"}}`

// Client is an MCP client connected to an upstream server. It speaks Streamable
// HTTP or legacy SSE (auto-detected by Connect) and uses no client-side
// timeouts so that hangs and cancellations originate visibly on the server or
// network side, not in the client.
//
// The only timeout in the whole code path is the auto-detect probe (30s) used
// by Connect to decide between Streamable HTTP and legacy SSE. After detection,
// every request flows through Client.client which has Timeout: 0; cancellation
// comes from the parent context only (Ctrl-C in CLI/REPL contexts).
//
// Construct via NewClient, then call Connect to do the handshake. Use the
// high-level helpers (ListTools, CallTool, …) or SendRequest as an escape
// hatch.
type Client struct {
	ctx    context.Context
	tc     *TransportConfig
	client *http.Client

	// Verbose controls whether every outbound/inbound JSON-RPC message is
	// pretty-printed to stderr. The REPL sets this to true; CLI mode keeps
	// it false unless the user passes -v.
	Verbose bool

	transport string // "http" or "sse"

	sessMu     sync.Mutex
	sessionID  string // streamable HTTP
	sessionURL string // legacy SSE POST endpoint

	pendMu  sync.Mutex
	pending map[string]chan json.RawMessage

	notifCh   chan json.RawMessage
	requestCh chan json.RawMessage // server-initiated requests (e.g. ping)

	nextID atomic.Int64
}

// NewClient creates an unconnected Client. Call Connect before issuing requests.
func NewClient(ctx context.Context, tc *TransportConfig) *Client {
	return &Client{
		ctx:       ctx,
		tc:        tc,
		client:    NewHTTPClient(tc.Insecure, 0),
		pending:   make(map[string]chan json.RawMessage),
		notifCh:   make(chan json.RawMessage, 64),
		requestCh: make(chan json.RawMessage, 64),
	}
}

// Connect establishes the transport (auto, http, or sse) and runs the MCP
// initialize handshake. Notifications are pumped into Client.notifCh and, when
// Verbose is true, printed to stderr.
func (c *Client) Connect() error {
	go c.notificationDrain()
	go c.requestDrain()

	switch c.tc.Type {
	case "sse":
		if err := c.connectLegacySSE(); err != nil {
			return fmt.Errorf("legacy SSE connect: %w", err)
		}
		return c.Initialize()
	case "http":
		c.transport = "http"
		log.Println("transport: Streamable HTTP (forced)")
		go c.streamableNotifListener()
		return c.Initialize()
	default:
		return c.autoInitialize()
	}
}

// Initialize runs the MCP initialize request and the matching
// notifications/initialized notification. Connect calls this for you; call it
// directly only if you want to re-handshake on an existing connection.
func (c *Client) Initialize() error {
	if _, err := c.SendRequest("initialize", json.RawMessage(debugInitParams)); err != nil {
		return err
	}
	return c.SendNotification("notifications/initialized", json.RawMessage(`{}`))
}

// ListTools sends tools/list. cursor may be "" for the first page.
func (c *Client) ListTools(cursor string) (json.RawMessage, error) {
	return c.unwrapResult(c.SendRequest("tools/list", listParams(cursor)))
}

// ListResources sends resources/list. cursor may be "" for the first page.
func (c *Client) ListResources(cursor string) (json.RawMessage, error) {
	return c.unwrapResult(c.SendRequest("resources/list", listParams(cursor)))
}

// ListPrompts sends prompts/list. cursor may be "" for the first page.
func (c *Client) ListPrompts(cursor string) (json.RawMessage, error) {
	return c.unwrapResult(c.SendRequest("prompts/list", listParams(cursor)))
}

// CallTool invokes tools/call with the given name and JSON arguments. Pass
// nil/empty args to send an empty object.
func (c *Client) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	p, err := json.Marshal(map[string]json.RawMessage{
		"name":      json.RawMessage(strconv.Quote(name)),
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}
	return c.unwrapResult(c.SendRequest("tools/call", p))
}

// Ping sends a ping request and returns the parsed result.
func (c *Client) Ping() (json.RawMessage, error) {
	return c.unwrapResult(c.SendRequest("ping", json.RawMessage(`{}`)))
}

// Raw sends a verbatim JSON-RPC envelope. If the envelope has an id, Raw waits
// for and returns the matching reply; otherwise it returns nil after the
// request is dispatched.
func (c *Client) Raw(envelope []byte) (json.RawMessage, error) {
	envelope = bytes.TrimSpace(envelope)
	if len(envelope) == 0 {
		return nil, fmt.Errorf("empty envelope")
	}
	if !json.Valid(envelope) {
		return nil, fmt.Errorf("invalid JSON")
	}
	var peek struct {
		ID json.RawMessage `json:"id"`
	}
	json.Unmarshal(envelope, &peek)
	hasID := len(peek.ID) > 0 && string(peek.ID) != "null"

	if !hasID {
		c.printOutbound(envelope)
		return nil, c.send(envelope)
	}

	idKey := string(peek.ID)
	respCh := make(chan json.RawMessage, 1)
	c.pendMu.Lock()
	c.pending[idKey] = respCh
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, idKey)
		c.pendMu.Unlock()
	}()

	c.printOutbound(envelope)
	start := time.Now()
	if err := c.send(envelope); err != nil {
		return nil, err
	}
	select {
	case data := <-respCh:
		c.printInbound(data, time.Since(start))
		return data, nil
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}

// REPL runs the interactive read-eval-print loop on stdin/stderr until the user
// types "quit" or stdin closes or the context is cancelled. The Client must be
// connected first. Verbose is forced on while the REPL runs.
func (c *Client) REPL() {
	prevVerbose := c.Verbose
	c.Verbose = true
	defer func() { c.Verbose = prevVerbose }()

	c.printREPLHelp()
	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	fmt.Fprint(os.Stderr, "mcp> ")
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				fmt.Fprint(os.Stderr, "mcp> ")
				continue
			}
			if c.runCommand(line) {
				return
			}
			fmt.Fprint(os.Stderr, "mcp> ")
		case <-c.ctx.Done():
			return
		}
	}
}

// SendRequest is the low-level escape hatch: marshals a JSON-RPC request with
// an auto-allocated id, dispatches via the active transport, and waits for the
// matching reply. Most callers want the typed helpers above.
func (c *Client) SendRequest(method string, params json.RawMessage) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	idJSON := json.RawMessage(strconv.FormatInt(id, 10))
	idKey := string(idJSON)

	body, err := json.Marshal(jsonRPCMessage{
		JSONRPC: "2.0",
		ID:      idJSON,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, err
	}

	respCh := make(chan json.RawMessage, 1)
	c.pendMu.Lock()
	c.pending[idKey] = respCh
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, idKey)
		c.pendMu.Unlock()
	}()

	c.printOutbound(body)
	start := time.Now()
	if err := c.send(body); err != nil {
		return nil, err
	}
	select {
	case data := <-respCh:
		c.printInbound(data, time.Since(start))
		return data, nil
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}

// SendNotification sends a JSON-RPC notification (no id, no reply expected).
func (c *Client) SendNotification(method string, params json.RawMessage) error {
	body, err := json.Marshal(jsonRPCMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}
	c.printOutbound(body)
	return c.send(body)
}

// listParams returns a params object containing only a cursor (or empty).
func listParams(cursor string) json.RawMessage {
	if cursor == "" {
		return json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(map[string]string{"cursor": cursor})
	return json.RawMessage(b)
}

// unwrapResult parses a JSON-RPC reply and returns the result field or, if the
// server signaled an error, an error wrapping its code+message.
func (c *Client) unwrapResult(resp json.RawMessage, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	var msg jsonRPCMessage
	if err := json.Unmarshal(resp, &msg); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("server error %d: %s", msg.Error.Code, msg.Error.Message)
	}
	return msg.Result, nil
}

// autoInitialize probes Streamable HTTP first and falls back to legacy SSE.
// The probe POST uses a 30s timeout client purely to bound auto-detection — the
// session client used afterwards has no timeout.
func (c *Client) autoInitialize() error {
	c.transport = "http"
	log.Printf("auto-detect: probing Streamable HTTP at %s", c.tc.ServerURL)

	id := c.nextID.Add(1)
	idJSON := json.RawMessage(strconv.FormatInt(id, 10))
	idKey := string(idJSON)
	body, _ := json.Marshal(jsonRPCMessage{
		JSONRPC: "2.0",
		ID:      idJSON,
		Method:  "initialize",
		Params:  json.RawMessage(debugInitParams),
	})

	c.printOutbound(body)
	start := time.Now()
	probeClient := NewHTTPClient(c.tc.Insecure, 30*time.Second)
	resp, err := PostMessage(c.ctx, probeClient, c.tc.ServerURL, "", c.tc.Headers, body)
	if err == nil && resp.StatusCode == 200 {
		log.Println("transport: Streamable HTTP")
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			c.sessMu.Lock()
			c.sessionID = sid
			c.sessMu.Unlock()
			log.Printf("session ID: %s", sid)
		}
		respCh := make(chan json.RawMessage, 1)
		c.pendMu.Lock()
		c.pending[idKey] = respCh
		c.pendMu.Unlock()
		go c.consumeResponseBody(resp)
		select {
		case data := <-respCh:
			c.printInbound(data, time.Since(start))
		case <-time.After(30 * time.Second):
			log.Println("initialize response timed out (30s)")
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
		c.pendMu.Lock()
		delete(c.pending, idKey)
		c.pendMu.Unlock()
		go c.streamableNotifListener()
		return c.SendNotification("notifications/initialized", json.RawMessage(`{}`))
	}
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		log.Printf("Streamable HTTP returned %d — falling back to legacy SSE", resp.StatusCode)
	} else {
		log.Printf("Streamable HTTP failed: %v — falling back to legacy SSE", err)
	}

	if err := c.connectLegacySSE(); err != nil {
		return fmt.Errorf("legacy SSE connect: %w", err)
	}
	if _, err := c.SendRequest("initialize", json.RawMessage(debugInitParams)); err != nil {
		return err
	}
	return c.SendNotification("notifications/initialized", json.RawMessage(`{}`))
}

func (c *Client) connectLegacySSE() error {
	c.transport = "sse"
	log.Printf("transport: legacy SSE — connecting to %s", c.tc.ServerURL)

	req, err := http.NewRequestWithContext(c.ctx, "GET", c.tc.ServerURL, nil)
	if err != nil {
		return err
	}
	applyHeaders(req, c.tc.Headers)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return fmt.Errorf("SSE GET returned status %d", resp.StatusCode)
	}
	log.Println("legacy SSE stream connected")

	endpointCh := make(chan string, 1)
	go func() {
		defer resp.Body.Close()
		err := parseSSE(resp.Body, func(event, data string) {
			switch event {
			case "endpoint":
				resolved, err := resolveSessionURL(c.tc.ServerURL, data)
				if err != nil {
					log.Printf("invalid endpoint URL %q: %v", data, err)
					return
				}
				select {
				case endpointCh <- resolved:
				default:
				}
			case "message":
				c.dispatch([]byte(data))
			}
		})
		if err != nil && c.ctx.Err() == nil {
			log.Printf("legacy SSE stream error: %v", err)
		}
	}()

	select {
	case sessURL := <-endpointCh:
		c.sessMu.Lock()
		c.sessionURL = sessURL
		c.sessMu.Unlock()
		log.Printf("legacy SSE session URL: %s", sessURL)
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for endpoint event (10s)")
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *Client) streamableNotifListener() {
	select {
	case <-time.After(200 * time.Millisecond):
	case <-c.ctx.Done():
		return
	}
	c.sessMu.Lock()
	sid := c.sessionID
	c.sessMu.Unlock()
	if sid == "" {
		log.Println("notification listener: no session ID, skipping")
		return
	}

	req, err := http.NewRequestWithContext(c.ctx, "GET", c.tc.ServerURL, nil)
	if err != nil {
		log.Printf("notification listener: %v", err)
		return
	}
	applyHeaders(req, c.tc.Headers)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Mcp-Session-Id", sid)

	resp, err := c.client.Do(req)
	if err != nil {
		if c.ctx.Err() == nil {
			log.Printf("notification listener: %v", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("notification listener: server returned %d (no GET SSE support)", resp.StatusCode)
		return
	}
	log.Println("notification listener connected")

	parseSSE(resp.Body, func(event, data string) {
		if event == "message" || event == "" {
			c.dispatch([]byte(data))
		}
	})
}

func (c *Client) send(body []byte) error {
	switch c.transport {
	case "http":
		return c.sendHTTP(body)
	case "sse":
		return c.sendLegacySSEPost(body)
	default:
		return fmt.Errorf("transport not configured")
	}
}

func (c *Client) sendHTTP(body []byte) error {
	c.sessMu.Lock()
	sid := c.sessionID
	c.sessMu.Unlock()
	resp, err := PostMessage(c.ctx, c.client, c.tc.ServerURL, sid, c.tc.Headers, body)
	if err != nil {
		return err
	}
	if newSID := resp.Header.Get("Mcp-Session-Id"); newSID != "" {
		c.sessMu.Lock()
		if c.sessionID == "" {
			c.sessionID = newSID
			log.Printf("session ID: %s", newSID)
		}
		c.sessMu.Unlock()
	}
	if resp.StatusCode == 202 {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	go c.consumeResponseBody(resp)
	return nil
}

func (c *Client) sendLegacySSEPost(body []byte) error {
	c.sessMu.Lock()
	url := c.sessionURL
	c.sessMu.Unlock()
	if url == "" {
		return fmt.Errorf("legacy SSE: no session URL established")
	}
	req, err := http.NewRequestWithContext(c.ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	applyHeaders(req, c.tc.Headers)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("legacy SSE POST: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) consumeResponseBody(resp *http.Response) {
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		err := parseSSE(resp.Body, func(event, data string) {
			if event == "message" || event == "" {
				c.dispatch([]byte(data))
			}
		})
		resp.Body.Close()
		if err != nil && c.ctx.Err() == nil {
			log.Printf("response stream error: %v", err)
		}
	case strings.HasPrefix(ct, "application/json"):
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("read body: %v", err)
			return
		}
		body = bytes.TrimSpace(body)
		if len(body) > 0 {
			c.dispatch(body)
		}
	default:
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return
		}
		body = bytes.TrimSpace(body)
		if len(body) > 0 && body[0] == '{' {
			c.dispatch(body)
		}
	}
}

func (c *Client) dispatch(data []byte) {
	var peek struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	json.Unmarshal(data, &peek)
	hasID := len(peek.ID) > 0 && string(peek.ID) != "null"

	// Server-initiated request: id + method present. Per JSON-RPC, these
	// require a reply (e.g. ping liveness checks). Route to requestDrain
	// instead of mislabelling as a notification.
	if hasID && peek.Method != "" {
		select {
		case c.requestCh <- data:
		default:
			log.Println("inbound-request buffer full, dropping message")
		}
		return
	}

	if hasID {
		c.pendMu.Lock()
		ch, ok := c.pending[string(peek.ID)]
		c.pendMu.Unlock()
		if ok {
			select {
			case ch <- data:
			default:
			}
			return
		}
	}
	select {
	case c.notifCh <- data:
	default:
		log.Println("notification buffer full, dropping message")
	}
}

func (c *Client) notificationDrain() {
	for {
		select {
		case data := <-c.notifCh:
			if c.Verbose {
				fmt.Fprintf(os.Stderr, "%s ← notification:\n%s\n", time.Now().Format("15:04:05.000"), prettyJSON(data))
			}
		case <-c.ctx.Done():
			return
		}
	}
}

// requestDrain handles server-initiated JSON-RPC requests. The protocol
// requires a reply for any frame with both `id` and `method` set (the
// canonical case is `ping` — without a reply some servers will eventually
// drop the connection). We answer `ping` with an empty result and reject
// anything else with -32601 so the server doesn't hang waiting.
func (c *Client) requestDrain() {
	for {
		select {
		case data := <-c.requestCh:
			c.handleServerRequest(data)
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) handleServerRequest(data []byte) {
	if c.Verbose {
		fmt.Fprintf(os.Stderr, "%s ← request:\n%s\n", time.Now().Format("15:04:05.000"), prettyJSON(data))
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("inbound request: parse error: %v", err)
		return
	}

	var reply []byte
	var err error
	switch msg.Method {
	case "ping":
		reply, err = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  struct{}        `json:"result"`
		}{JSONRPC: "2.0", ID: msg.ID})
	default:
		reply, err = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error: struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -32601, Message: "method not implemented by client: " + msg.Method},
		})
	}
	if err != nil {
		log.Printf("inbound request: marshal reply: %v", err)
		return
	}

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "%s → response:\n%s\n", time.Now().Format("15:04:05.000"), prettyJSON(reply))
	}
	if err := c.send(reply); err != nil {
		log.Printf("inbound request: send reply: %v", err)
	}
}

// REPL command dispatch

func (c *Client) runCommand(line string) (quit bool) {
	cmd, rest := splitCommand(line)
	switch cmd {
	case "quit", "exit":
		return true
	case "help", "?":
		c.printREPLHelp()
	case "headers":
		c.sessMu.Lock()
		fmt.Fprintf(os.Stderr, "transport=%s session_id=%q session_url=%q\n", c.transport, c.sessionID, c.sessionURL)
		c.sessMu.Unlock()
		for k, v := range c.tc.Headers {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", k, v)
		}
	case "init", "initialize":
		if err := c.Initialize(); err != nil {
			log.Printf("initialize: %v", err)
		}
	case "tools":
		if _, err := c.SendRequest("tools/list", json.RawMessage(`{}`)); err != nil {
			log.Printf("tools/list: %v", err)
		}
	case "resources":
		if _, err := c.SendRequest("resources/list", json.RawMessage(`{}`)); err != nil {
			log.Printf("resources/list: %v", err)
		}
	case "prompts":
		if _, err := c.SendRequest("prompts/list", json.RawMessage(`{}`)); err != nil {
			log.Printf("prompts/list: %v", err)
		}
	case "ping":
		if _, err := c.SendRequest("ping", json.RawMessage(`{}`)); err != nil {
			log.Printf("ping: %v", err)
		}
	case "call":
		toolName, argStr := splitCommand(rest)
		if toolName == "" {
			log.Println("usage: call <tool-name> <json-args | @path/to/args.json>")
			return false
		}
		argStr = strings.TrimSpace(argStr)
		if argStr == "" {
			argStr = "{}"
		}
		if strings.HasPrefix(argStr, "@") {
			loaded, err := loadREPLJSONArgs(argStr)
			if err != nil {
				log.Printf("call: %v", err)
				return false
			}
			argStr = loaded
		}
		if !json.Valid([]byte(argStr)) {
			log.Printf("invalid JSON args\n%s", diagnoseJSONArgs(argStr))
			return false
		}
		if _, err := c.CallTool(toolName, json.RawMessage(argStr)); err != nil {
			log.Printf("tools/call: %v", err)
		}
	case "raw":
		rest = strings.TrimSpace(rest)
		if rest == "" {
			log.Println("usage: raw <json-rpc-envelope>")
			return false
		}
		if _, err := c.Raw([]byte(rest)); err != nil {
			log.Printf("raw: %v", err)
		}
	default:
		log.Printf("unknown command %q (type 'help')", cmd)
	}
	return false
}

// loadREPLJSONArgs reads JSON args from a file when the REPL user types
// "@path". The "@file" form sidesteps shell-quoting issues and also lets the
// REPL accept multi-line JSON payloads from disk.
func loadREPLJSONArgs(s string) (string, error) {
	path := strings.TrimPrefix(s, "@")
	if path == "" {
		return "", fmt.Errorf("missing file path after '@'")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read JSON args from %s: %w", path, err)
	}
	return string(data), nil
}

// jsonArgHint mirrors the CLI hint shown when JSON args fail to parse. We
// duplicate it across the package boundary deliberately — the strings drift
// independently because the REPL has slightly different ergonomics (no shell
// to misquote things, but multi-line input matters more), and importing
// across `cmd` ↔ `internal` would require lifting it into a third package
// for a five-line constant.
const jsonArgHint = `Pass JSON args one of these ways:
  Inline:                   {"key":1}        (REPL doesn't strip quotes)
  Multi-line / from file:   @args.json       (read JSON from disk)`

// barewordKeyRE detects the "{barekey:value" symptom — the user pasted JSON
// from a shell that stripped its double quotes. In the REPL this is rare
// (no shell in the loop) but worth catching for users who paste pre-mangled
// snippets from elsewhere.
var barewordKeyRE = regexp.MustCompile(`^\s*\{\s*[A-Za-z_][A-Za-z0-9_]*\s*:`)

var quoteBarewordKeysRE = regexp.MustCompile(`([\{,]\s*)([A-Za-z_][A-Za-z0-9_]*)(\s*:)`)

// diagnoseJSONArgs builds a multi-line debug report for invalid JSON args
// in the REPL. Same shape as the CLI version: bytes received, parse error,
// "did you mean?" reconstruction when we can recover, and the input hint.
func diagnoseJSONArgs(input string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "received %d bytes: %s\n", len(input), quoteForDisplay(input))
	if err := json.Unmarshal([]byte(input), new(json.RawMessage)); err != nil {
		fmt.Fprintf(&b, "parse error: %v\n", err)
	}
	if barewordKeyRE.MatchString(input) {
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "→ JSON keys are missing their surrounding double quotes.")
		fmt.Fprintln(&b, "  If you copied this from a Windows shell, the shell stripped them before the REPL ever saw the input.")
	}
	if fixed, ok := tryRecoverJSON(input); ok {
		fmt.Fprintln(&b, "")
		fmt.Fprintf(&b, "did you mean: %s\n", fixed)
	}
	fmt.Fprintln(&b, "")
	fmt.Fprint(&b, jsonArgHint)
	return b.String()
}

func tryRecoverJSON(input string) (string, bool) {
	candidate := quoteBarewordKeysRE.ReplaceAllString(input, `$1"$2"$3`)
	if candidate == input {
		return "", false
	}
	if json.Valid([]byte(candidate)) {
		return candidate, true
	}
	return "", false
}

func quoteForDisplay(s string) string {
	var b strings.Builder
	b.WriteString("«")
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			b.WriteString(`\x` + strconv.FormatInt(int64(r), 16))
		}
	}
	b.WriteString("»")
	return b.String()
}

func splitCommand(line string) (cmd, rest string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

func (c *Client) printREPLHelp() {
	fmt.Fprintln(os.Stderr, `Commands:
  init                 Run the MCP initialize handshake
  tools                List tools (tools/list)
  resources            List resources (resources/list)
  prompts              List prompts (prompts/list)
  ping                 Send a ping request
  call <name> <json>   Call a tool. Examples:
                         call prtg_get_sensors {"compact":true,"limit":1}
                         call prtg_get_sensors @args.json
  raw <json>           Send a raw JSON-RPC envelope verbatim
  headers              Show transport, session, and HTTP headers
  help                 This help
  quit                 Exit (Ctrl-C also works; no client-side timeouts)`)
}

func (c *Client) printOutbound(body []byte) {
	if !c.Verbose {
		return
	}
	fmt.Fprintf(os.Stderr, "%s → outbound:\n%s\n", time.Now().Format("15:04:05.000"), prettyJSON(body))
}

func (c *Client) printInbound(body []byte, elapsed time.Duration) {
	if !c.Verbose {
		return
	}
	fmt.Fprintf(os.Stderr, "%s ← response in %.2fms:\n%s\n", time.Now().Format("15:04:05.000"), float64(elapsed.Microseconds())/1000.0, prettyJSON(body))
}

func prettyJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "  ", "  "); err != nil {
		return "  " + string(data)
	}
	return "  " + buf.String()
}

// RunDebugClient is a convenience wrapper that creates a Client, connects, and
// runs the interactive REPL. Existing callers (the --debug flag) keep working.
func RunDebugClient(ctx context.Context, tc *TransportConfig) error {
	c := NewClient(ctx, tc)
	c.Verbose = true
	if err := c.Connect(); err != nil {
		log.Printf("connect failed: %v", err)
		// Drop into REPL anyway so the user can investigate.
	}
	c.REPL()
	return nil
}
