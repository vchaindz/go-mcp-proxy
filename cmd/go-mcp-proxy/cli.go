package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"

	"go-mcp-proxy/internal/proxy"
)

// defaultPrintLimit is how many items we print by default for list commands and
// for arrays embedded in tool-call results. PRTG and similar systems can have
// 10K+ entries, so a small default keeps the terminal usable; --full or
// --limit 0 lifts the cap.
const defaultPrintLimit = 50

// knownVerbs decides whether the first positional after global flags is a CLI
// subcommand (rather than a server URL for proxy mode).
var knownVerbs = map[string]bool{
	"tools":     true,
	"resources": true,
	"prompts":   true,
	"call":      true,
	"ping":      true,
	"raw":       true,
	"debug":     true,
}

// runCLI dispatches a subcommand. args is the slice AFTER the verb. Returns
// nil on success or an error to be reported to the user.
func runCLI(verb string, args []string, transType string, insecure bool, headers proxy.HeaderFlag) error {
	switch verb {
	case "tools", "resources", "prompts":
		return cmdList(args, verb, transType, insecure, headers)
	case "call":
		return cmdCall(args, transType, insecure, headers)
	case "ping":
		return cmdPing(args, transType, insecure, headers)
	case "raw":
		return cmdRaw(args, transType, insecure, headers)
	case "debug":
		return cmdDebug(args, transType, insecure, headers)
	}
	return fmt.Errorf("unknown verb %q", verb)
}

// loadJSONArgs returns s unchanged, or if s starts with "@", reads the file at
// s[1:] and returns its contents. The "@file" form sidesteps shell-quoting
// issues — particularly on Windows cmd.exe, where single quotes are literal
// and inner double quotes get stripped by the shell.
func loadJSONArgs(s string) (string, error) {
	if !strings.HasPrefix(s, "@") {
		return s, nil
	}
	path := s[1:]
	if path == "" {
		return "", errors.New("@: missing file path after '@'")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read JSON args from %s: %w", path, err)
	}
	return string(data), nil
}

// jsonArgHint is the multi-line hint shown when JSON args fail to parse. Most
// of the time the failure is shell-quoting on Windows; we cover the three
// common patterns so the user can pick one that works in their shell.
const jsonArgHint = `Pass JSON args one of three ways:
  Unix shell:    '{"key":1}'
  Windows cmd:   "{\"key\":1}"
  Any shell:     @args.json   (reads JSON from a file — no quoting needed)`

// parseFlexible lets users freely interleave positionals and flags within a
// subcommand. The stdlib flag package stops at the first non-flag, so we loop:
// parse → take one positional → parse the rest → repeat.
func parseFlexible(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	fs.SetOutput(io.Discard)
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

func makeContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		log.Println("signal received, shutting down")
		cancel()
	}()
	return ctx, cancel
}

func newConnectedClient(ctx context.Context, url, transType string, insecure bool, headers proxy.HeaderFlag, verbose bool) (*proxy.Client, error) {
	if url == "" {
		return nil, errors.New("URL required")
	}
	tc := &proxy.TransportConfig{
		ServerURL: url,
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
	c := proxy.NewClient(ctx, tc)
	c.Verbose = verbose
	if err := c.Connect(); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return c, nil
}

// cmdList handles tools / resources / prompts.
func cmdList(args []string, kind, transType string, insecure bool, headers proxy.HeaderFlag) error {
	fs := flag.NewFlagSet(kind, flag.ContinueOnError)
	var (
		limit   int
		cursor  string
		full    bool
		rawJSON bool
		verbose bool
	)
	fs.IntVar(&limit, "limit", defaultPrintLimit, "max items to display (0 = no cap)")
	fs.StringVar(&cursor, "cursor", "", "MCP pagination cursor (use the next-cursor from a previous run)")
	fs.BoolVar(&full, "full", false, "print full JSON for each item")
	fs.BoolVar(&rawJSON, "json", false, "emit the raw JSON result instead of formatted output")
	fs.BoolVar(&verbose, "v", false, "verbose: print every JSON-RPC frame to stderr")

	positionals, err := parseFlexible(fs, args)
	if err != nil {
		return err
	}
	if len(positionals) < 1 {
		return fmt.Errorf("usage: %s <url> [--limit N] [--cursor TOK] [--full] [--json]", kind)
	}
	url := positionals[0]

	ctx, cancel := makeContext()
	defer cancel()
	c, err := newConnectedClient(ctx, url, transType, insecure, headers, verbose)
	if err != nil {
		return err
	}

	var (
		result  json.RawMessage
		listErr error
	)
	switch kind {
	case "tools":
		result, listErr = c.ListTools(cursor)
	case "resources":
		result, listErr = c.ListResources(cursor)
	case "prompts":
		result, listErr = c.ListPrompts(cursor)
	}
	if listErr != nil {
		return listErr
	}
	if rawJSON {
		return printJSON(result)
	}
	return printList(kind, result, limit, full)
}

// cmdCall handles `call <url> <tool> [json-args]`.
func cmdCall(args []string, transType string, insecure bool, headers proxy.HeaderFlag) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	var (
		rawJSON  bool
		full     bool
		maxLines int
		verbose  bool
	)
	fs.BoolVar(&rawJSON, "json", false, "emit the raw JSON result instead of formatted output")
	fs.BoolVar(&full, "full", false, "do not truncate output (text or arrays)")
	fs.IntVar(&maxLines, "lines", 200, "max output lines / array items per content block (0 = no cap)")
	fs.BoolVar(&verbose, "v", false, "verbose: print every JSON-RPC frame to stderr")

	positionals, err := parseFlexible(fs, args)
	if err != nil {
		return err
	}
	if len(positionals) < 2 {
		return fmt.Errorf(`usage: call <url> <tool-name> [json-args | @path/to/args.json]
example: call http://server prtg_get_sensors '{"compact":true,"limit":1}'
example: call http://server prtg_get_sensors @args.json`)
	}
	url := positionals[0]
	toolName := positionals[1]
	argStr := "{}"
	if len(positionals) >= 3 {
		loaded, err := loadJSONArgs(positionals[2])
		if err != nil {
			return err
		}
		argStr = loaded
	}
	if !json.Valid([]byte(argStr)) {
		return fmt.Errorf("invalid JSON args: %s\n\n%s", argStr, jsonArgHint)
	}

	ctx, cancel := makeContext()
	defer cancel()
	c, err := newConnectedClient(ctx, url, transType, insecure, headers, verbose)
	if err != nil {
		return err
	}

	result, err := c.CallTool(toolName, json.RawMessage(argStr))
	if err != nil {
		return err
	}
	if rawJSON {
		return printJSON(result)
	}
	return printCallResult(result, maxLines, full)
}

func cmdPing(args []string, transType string, insecure bool, headers proxy.HeaderFlag) error {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	var verbose bool
	fs.BoolVar(&verbose, "v", false, "verbose: print every JSON-RPC frame to stderr")
	positionals, err := parseFlexible(fs, args)
	if err != nil {
		return err
	}
	if len(positionals) < 1 {
		return fmt.Errorf("usage: ping <url>")
	}
	ctx, cancel := makeContext()
	defer cancel()
	c, err := newConnectedClient(ctx, positionals[0], transType, insecure, headers, verbose)
	if err != nil {
		return err
	}
	if _, err := c.Ping(); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdRaw(args []string, transType string, insecure bool, headers proxy.HeaderFlag) error {
	fs := flag.NewFlagSet("raw", flag.ContinueOnError)
	var verbose bool
	fs.BoolVar(&verbose, "v", false, "verbose: print every JSON-RPC frame to stderr")
	positionals, err := parseFlexible(fs, args)
	if err != nil {
		return err
	}
	if len(positionals) < 2 {
		return fmt.Errorf("usage: raw <url> <json-rpc-envelope>")
	}
	ctx, cancel := makeContext()
	defer cancel()
	c, err := newConnectedClient(ctx, positionals[0], transType, insecure, headers, verbose)
	if err != nil {
		return err
	}
	resp, err := c.Raw([]byte(positionals[1]))
	if err != nil {
		return err
	}
	if resp != nil {
		return printJSON(resp)
	}
	fmt.Fprintln(os.Stderr, "(notification sent; no reply expected)")
	return nil
}

func cmdDebug(args []string, transType string, insecure bool, headers proxy.HeaderFlag) error {
	fs := flag.NewFlagSet("debug", flag.ContinueOnError)
	positionals, err := parseFlexible(fs, args)
	if err != nil {
		return err
	}
	if len(positionals) < 1 {
		return fmt.Errorf("usage: debug <url>")
	}
	runDebugMode(positionals[0], transType, insecure, headers)
	return nil
}

// printJSON pretty-prints raw JSON to stdout.
func printJSON(data json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		os.Stdout.Write(data)
		os.Stdout.Write([]byte("\n"))
		return nil
	}
	os.Stdout.Write(buf.Bytes())
	os.Stdout.Write([]byte("\n"))
	return nil
}

// printList renders a tools/resources/prompts list compactly. The total count
// and any next-cursor are always printed so the operator knows when there's
// more to page through.
func printList(kind string, result json.RawMessage, limit int, full bool) error {
	var parsed struct {
		Tools      []json.RawMessage `json:"tools"`
		Resources  []json.RawMessage `json:"resources"`
		Prompts    []json.RawMessage `json:"prompts"`
		NextCursor string            `json:"nextCursor"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return fmt.Errorf("parse list result: %w", err)
	}
	var items []json.RawMessage
	switch kind {
	case "tools":
		items = parsed.Tools
	case "resources":
		items = parsed.Resources
	case "prompts":
		items = parsed.Prompts
	}

	total := len(items)
	shown := total
	if limit > 0 && total > limit {
		shown = limit
	}

	for i := 0; i < shown; i++ {
		printListItem(kind, items[i], full)
	}
	switch {
	case total == 0:
		fmt.Println("(no items)")
	case shown < total:
		fmt.Printf("... (showing %d of %d; pass --limit %d or --full for more)\n", shown, total, total)
	default:
		fmt.Printf("(%d items)\n", total)
	}
	if parsed.NextCursor != "" {
		fmt.Printf("next-cursor: %s\n", parsed.NextCursor)
	}
	return nil
}

func printListItem(kind string, item json.RawMessage, full bool) {
	if full {
		var b bytes.Buffer
		json.Indent(&b, item, "", "  ")
		fmt.Println(b.String())
		return
	}
	switch kind {
	case "tools", "prompts":
		var t struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		json.Unmarshal(item, &t)
		desc := truncate(strings.TrimSpace(strings.SplitN(t.Description, "\n", 2)[0]), 80)
		fmt.Printf("%-32s  %s\n", t.Name, desc)
	case "resources":
		var r struct {
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			MimeType    string `json:"mimeType"`
		}
		json.Unmarshal(item, &r)
		// URI is the canonical resource identifier — surface it first so the
		// operator can copy/paste into a follow-up resources/read or call.
		line := r.URI
		if r.Name != "" && r.Name != r.URI {
			line += "  " + r.Name
		}
		if r.MimeType != "" {
			line += "  [" + r.MimeType + "]"
		}
		fmt.Println(line)
	default:
		var b bytes.Buffer
		json.Indent(&b, item, "", "  ")
		fmt.Println(b.String())
	}
}

// printCallResult renders a tools/call result. MCP shape is
// {"content":[{"type":"text","text":"..."}, ...], "isError":bool}. Tools that
// pack large JSON (e.g. PRTG sensor lists) into the text get summarised.
func printCallResult(result json.RawMessage, maxLines int, full bool) error {
	var parsed struct {
		Content []struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return printJSON(result)
	}
	if parsed.IsError {
		fmt.Fprintln(os.Stderr, "tool reported isError=true:")
	}
	if len(parsed.Content) == 0 {
		return printJSON(result)
	}
	for i, block := range parsed.Content {
		if len(parsed.Content) > 1 {
			fmt.Printf("--- content[%d] type=%s\n", i, block.Type)
		}
		switch block.Type {
		case "text", "":
			printTextBlock(block.Text, maxLines, full)
		default:
			raw, _ := json.MarshalIndent(block, "", "  ")
			os.Stdout.Write(raw)
			fmt.Println()
		}
	}
	return nil
}

// printTextBlock prints a content text. If it parses as JSON containing an
// array (or a recognised wrapper around one), the array is summarised one
// item per line by id/name fields. Otherwise the text is printed, capped to
// maxLines unless full is set.
func printTextBlock(text string, maxLines int, full bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if c := text[0]; c == '{' || c == '[' {
		var anyJSON any
		if err := json.Unmarshal([]byte(text), &anyJSON); err == nil {
			if printIfArray(anyJSON, maxLines, full) {
				return
			}
			var b bytes.Buffer
			json.Indent(&b, []byte(text), "", "  ")
			printLines(b.String(), maxLines, full)
			return
		}
	}
	printLines(text, maxLines, full)
}

// printIfArray detects an array (or an object wrapping one under common keys)
// and prints a compact summary. Returns true if it consumed v.
func printIfArray(v any, maxLines int, full bool) bool {
	arr, ok := v.([]any)
	if !ok {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		var wrapperKey string
		for _, key := range []string{"items", "data", "results", "rows", "sensors", "alerts", "groups", "tags"} {
			if a, ok := m[key].([]any); ok {
				arr = a
				wrapperKey = key
				break
			}
		}
		if arr == nil {
			return false
		}
		fmt.Printf("(%s wrapper, %d items)\n", wrapperKey, len(arr))
	}
	total := len(arr)
	if full {
		b, _ := json.MarshalIndent(arr, "", "  ")
		os.Stdout.Write(b)
		fmt.Println()
		return true
	}
	show := total
	if maxLines > 0 && show > maxLines {
		show = maxLines
	}
	for i := 0; i < show; i++ {
		fmt.Println(compactArrayItem(arr[i]))
	}
	switch {
	case total == 0:
		fmt.Println("(empty array)")
	case show < total:
		fmt.Printf("... (showing %d of %d items; pass --full or --lines %d to see all; pass a `limit` argument to the tool to reduce server load)\n", show, total, total)
	default:
		fmt.Printf("(%d items)\n", total)
	}
	return true
}

// compactArrayItem renders one element of a JSON array using common ID/name
// fields (id, objid, sensor_id, uri, name, …). Falls back to a JSON snippet.
func compactArrayItem(item any) string {
	m, ok := item.(map[string]any)
	if !ok {
		b, _ := json.Marshal(item)
		return truncate(string(b), 120)
	}
	idKey, idVal := findFirstField(m, "id", "objid", "object_id", "sensor_id", "uri", "uid")
	nameKey, nameVal := findFirstField(m, "name", "sensor_name", "device", "device_name", "title", "label")
	hasID := idKey != ""
	hasName := nameKey != ""
	switch {
	case hasID && hasName:
		return fmt.Sprintf("%s=%v  %s=%v", idKey, idVal, nameKey, nameVal)
	case hasID:
		return fmt.Sprintf("%s=%v", idKey, idVal)
	case hasName:
		return fmt.Sprintf("%s=%v", nameKey, nameVal)
	default:
		b, _ := json.Marshal(item)
		return truncate(string(b), 120)
	}
}

func findFirstField(m map[string]any, keys ...string) (string, any) {
	for _, k := range keys {
		if v, ok := m[k]; ok && !isZeroish(v) {
			return k, v
		}
	}
	return "", nil
}

func isZeroish(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	}
	return false
}

func printLines(s string, maxLines int, full bool) {
	if full || maxLines <= 0 {
		fmt.Println(s)
		return
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		fmt.Println(s)
		return
	}
	fmt.Println(strings.Join(lines[:maxLines], "\n"))
	fmt.Printf("... (truncated to %d of %d lines; pass --full to see all)\n", maxLines, len(lines))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
