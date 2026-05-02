package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"go-mcp-proxy/internal/proxy"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime)

	var (
		configPath string
		serverName string
		insecure   bool
		transType  string
		headers    proxy.HeaderFlag
		debugURL   string
	)

	flag.StringVar(&configPath, "config", "", "path to JSON config file")
	flag.StringVar(&serverName, "server", "", "server name from config (default: first/only server)")
	flag.BoolVar(&insecure, "insecure", false, "skip TLS certificate verification")
	flag.StringVar(&transType, "type", "auto", "transport: http, sse, auto")
	flag.Var(&headers, "header", "custom header key=value (repeatable)")
	flag.StringVar(&debugURL, "debug", "", "interactive debug client: connect to URL and start a REPL (no client-side timeouts)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [global-flags] [<command> <args...> | <server-url>]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Without a command, runs as an stdin/stdout MCP proxy against <server-url>.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Commands:")
		fmt.Fprintln(os.Stderr, "  tools <url>                  List tools (tools/list)")
		fmt.Fprintln(os.Stderr, "  resources <url>              List resources (resources/list)")
		fmt.Fprintln(os.Stderr, "  prompts <url>                List prompts (prompts/list)")
		fmt.Fprintln(os.Stderr, "  call <url> <name> [args|@file]")
		fmt.Fprintln(os.Stderr, "                               Call a tool. args is JSON (default {}); @file reads JSON from disk.")
		fmt.Fprintln(os.Stderr, "  ping <url>                   Ping the server")
		fmt.Fprintln(os.Stderr, "  raw <url> <envelope>         Send a raw JSON-RPC envelope")
		fmt.Fprintln(os.Stderr, "  debug <url>                  Interactive REPL")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Per-command flags: --limit, --cursor, --full, --json, --lines, -v")
		fmt.Fprintln(os.Stderr, "(see `<cmd> -h` … for now run with no args after the verb to see usage)")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Global flags:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Supports both Streamable HTTP and legacy SSE transports.")
		fmt.Fprintln(os.Stderr, "Transport is auto-detected unless -type is specified.")
	}
	flag.Parse()

	// Subcommand dispatch: if the first positional looks like a verb (no
	// scheme separator), treat it as a CLI command. Known verbs run; unknown
	// non-URL tokens error clearly so typos don't fall through into proxy mode.
	if flag.NArg() > 0 {
		first := flag.Arg(0)
		if !strings.Contains(first, "://") {
			if knownVerbs[first] {
				if err := runCLI(first, flag.Args()[1:], transType, insecure, headers); err != nil {
					log.Fatalf("%s: %v", first, err)
				}
				return
			}
			log.Fatalf("unknown command %q (commands: tools, resources, prompts, call, ping, raw, debug)", first)
		}
	}

	// Debug client mode: short-circuit the proxy entirely and run a REPL against
	// the given URL. Reuses --header, --insecure, --type. No config file, no
	// stdin proxying.
	if debugURL != "" {
		runDebugMode(debugURL, transType, insecure, headers)
		return
	}

	// Build TransportConfig from flags, config file, and env vars.
	// Priority: CLI flags > config file > env vars
	tc := proxy.TransportConfig{
		Headers: make(map[string]string),
		Type:    transType,
	}

	// Positional arg for server URL
	if flag.NArg() > 0 {
		tc.ServerURL = flag.Arg(0)
	}

	// Load config file if provided
	if configPath != "" {
		cfg, err := proxy.LoadConfig(configPath)
		if err != nil {
			log.Fatalf("config error: %v", err)
		}

		var sc proxy.ServerConfig
		if serverName != "" {
			var ok bool
			sc, ok = cfg.MCPServers[serverName]
			if !ok {
				log.Fatalf("server %q not found in config", serverName)
			}
		} else {
			// Use first (or only) server
			for _, v := range cfg.MCPServers {
				sc = v
				break
			}
		}

		// Config file values fill in what CLI didn't set
		if tc.ServerURL == "" {
			tc.ServerURL = sc.URL
		}
		for k, v := range sc.Headers {
			if _, exists := tc.Headers[k]; !exists {
				tc.Headers[k] = v
			}
		}
		if sc.Insecure {
			insecure = true
		}
		if sc.Type != "" && transType == "auto" {
			tc.Type = sc.Type
		}
	}

	// CLI -header flags override config headers
	for k, v := range headers {
		tc.Headers[k] = v
	}
	tc.Insecure = insecure

	// Env var fallbacks (lowest priority)
	if tc.ServerURL == "" {
		if v := os.Getenv("MCP_SERVER_URL"); v != "" {
			tc.ServerURL = v
		} else if v := os.Getenv("MCP_SSE_URL"); v != "" {
			tc.ServerURL = v
		}
	}

	if v := os.Getenv("MCP_HEADERS"); v != "" {
		// MCP_HEADERS format: "Key1=Value1,Key2=Value2"
		for _, pair := range strings.Split(v, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			k, val, ok := strings.Cut(pair, "=")
			if !ok {
				log.Printf("warning: ignoring malformed MCP_HEADERS entry %q", pair)
				continue
			}
			k = strings.TrimSpace(k)
			val = strings.TrimSpace(val)
			if _, exists := tc.Headers[k]; !exists {
				tc.Headers[k] = val
			}
		}
	}

	if v := os.Getenv("MCP_AUTH_TOKEN"); v != "" {
		if _, exists := tc.Headers["Authorization"]; !exists {
			tc.Headers["Authorization"] = "Bearer " + v
		}
	}

	if !tc.Insecure {
		if v := os.Getenv("MCP_INSECURE"); v == "1" || strings.EqualFold(v, "true") {
			tc.Insecure = true
		}
	}

	if tc.Type == "auto" {
		if v := os.Getenv("MCP_TRANSPORT"); v == "http" || v == "sse" {
			tc.Type = v
		}
	}

	if tc.ServerURL == "" {
		flag.Usage()
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		log.Println("signal received, shutting down")
		cancel()
	}()

	// Read the first stdin line (should be the initialize request)
	stdinCh := make(chan []byte, 16)
	go proxy.StdinReader(ctx, stdinCh)

	var firstLine []byte
	select {
	case line, ok := <-stdinCh:
		if !ok {
			log.Fatal("stdin closed before receiving first message")
		}
		firstLine = line
	case <-time.After(30 * time.Second):
		log.Fatal("timeout waiting for first stdin message (30s)")
	case <-ctx.Done():
		return
	}

	firstLine = bytes.TrimSpace(firstLine)
	if len(firstLine) == 0 {
		log.Fatal("first stdin line is empty")
	}

	// Route based on transport type
	switch tc.Type {
	case "sse":
		log.Printf("using legacy SSE transport (forced) to %s", tc.ServerURL)
		proxy.RunLegacySSE(ctx, &tc, firstLine, stdinCh)
		return
	case "http":
		log.Printf("using Streamable HTTP transport (forced) to %s", tc.ServerURL)
		resp, err := proxy.PostMessage(ctx, proxy.NewHTTPClient(tc.Insecure, 30*time.Second), tc.ServerURL, "", tc.Headers, firstLine)
		if err != nil {
			log.Fatalf("Streamable HTTP POST failed: %v", err)
		}
		if resp.StatusCode != 200 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			log.Fatalf("Streamable HTTP returned status %d", resp.StatusCode)
		}
		proxy.RunStreamableHTTP(ctx, &tc, firstLine, resp, stdinCh)
		return
	}

	// Auto-detect: try Streamable HTTP first, fall back to legacy SSE
	log.Printf("attempting Streamable HTTP transport to %s", tc.ServerURL)
	resp, err := proxy.PostMessage(ctx, proxy.NewHTTPClient(tc.Insecure, 30*time.Second), tc.ServerURL, "", tc.Headers, firstLine)
	if err == nil && resp.StatusCode == 200 {
		log.Println("using Streamable HTTP transport")
		proxy.RunStreamableHTTP(ctx, &tc, firstLine, resp, stdinCh)
		return
	}

	// Close the response if we got one
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		log.Printf("Streamable HTTP returned status %d, falling back to legacy SSE", resp.StatusCode)
	} else {
		log.Printf("Streamable HTTP failed: %v, falling back to legacy SSE", err)
	}

	// Fall back to legacy SSE transport
	log.Printf("attempting legacy SSE transport to %s", tc.ServerURL)
	proxy.RunLegacySSE(ctx, &tc, firstLine, stdinCh)
}

// runDebugMode runs the interactive MCP debug REPL against the given URL.
// Auth tokens, custom headers, TLS skipping, and forced transport selection
// flow through the same flags as proxy mode (--header, --insecure, --type).
// MCP_AUTH_TOKEN / MCP_HEADERS / MCP_INSECURE env vars are honoured as fallbacks.
func runDebugMode(serverURL, transType string, insecure bool, headers proxy.HeaderFlag) {
	tc := proxy.TransportConfig{
		ServerURL: serverURL,
		Headers:   make(map[string]string),
		Insecure:  insecure,
		Type:      transType,
	}
	for k, v := range headers {
		tc.Headers[k] = v
	}

	if v := os.Getenv("MCP_HEADERS"); v != "" {
		for _, pair := range strings.Split(v, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			k, val, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			val = strings.TrimSpace(val)
			if _, exists := tc.Headers[k]; !exists {
				tc.Headers[k] = val
			}
		}
	}
	if v := os.Getenv("MCP_AUTH_TOKEN"); v != "" {
		if _, exists := tc.Headers["Authorization"]; !exists {
			tc.Headers["Authorization"] = "Bearer " + v
		}
	}
	if !tc.Insecure {
		if v := os.Getenv("MCP_INSECURE"); v == "1" || strings.EqualFold(v, "true") {
			tc.Insecure = true
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		log.Println("signal received, shutting down")
		cancel()
	}()

	if err := proxy.RunDebugClient(ctx, &tc); err != nil {
		log.Fatalf("debug client: %v", err)
	}
}
