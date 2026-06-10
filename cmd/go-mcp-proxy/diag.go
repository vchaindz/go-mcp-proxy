package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"go-mcp-proxy/internal/proxy"
)

// cmdDiag runs a full connectivity diagnostic against an MCP server and
// writes a single self-contained report file: environment, configuration
// (secrets redacted), the complete HTTP wire trace and JSON-RPC frames for
// every step, and a pass/fail summary. The report is the artifact to take
// along when leaving a customer site — everything needed to compare a
// working system against a failing one is in the file.
func cmdDiag(args []string, transType string, insecure bool, headers proxy.HeaderFlag) error {
	fs := flag.NewFlagSet("diag", flag.ContinueOnError)
	var outPath string
	fs.StringVar(&outPath, "o", "", "report file path (default: mcp-diag-<timestamp>.txt in the current directory)")
	positionals, err := parseFlexible(fs, args)
	if err != nil {
		return err
	}
	if len(positionals) < 1 {
		return fmt.Errorf(`usage: diag <url> [tool-name [json-args|@file]] [-o report.txt]
example: diag http://server:8443/mcp
example: diag http://server:8443/mcp prtg_get_sensor_status '{"sensor_id":65635}' -o customer-x.txt`)
	}
	serverURL := positionals[0]
	toolName := ""
	argStr := "{}"
	if len(positionals) >= 2 {
		toolName = positionals[1]
	}
	if len(positionals) >= 3 {
		loaded, err := loadJSONArgs(positionals[2])
		if err != nil {
			return err
		}
		argStr = loaded
	}
	if toolName != "" && !json.Valid([]byte(argStr)) {
		fixed, ok := tryRecoverJSON(argStr)
		if !ok {
			return fmt.Errorf("invalid JSON args\n%s", diagnoseJSONArgs(argStr))
		}
		fmt.Fprintln(os.Stderr, "note: auto-recovered shell-mangled JSON args")
		fmt.Fprintf(os.Stderr, "      original: %s\n", quoteForDisplay(argStr))
		fmt.Fprintf(os.Stderr, "      using:    %s\n", fixed)
		argStr = fixed
	}

	if outPath == "" {
		outPath = time.Now().Format("mcp-diag-20060102-150405.txt")
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create report file: %w", err)
	}
	defer f.Close()

	tc, err := buildTransportConfig(serverURL, transType, insecure, headers)
	if err != nil {
		return err
	}

	// Route everything into the report: HTTP wire trace, JSON-RPC frames,
	// and the transport log lines (auto-detect, session ID, …). Log lines
	// stay on stderr too so the user sees progress live.
	proxy.SetWireTrace(f)
	defer proxy.SetWireTrace(nil)
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	defer log.SetOutput(os.Stderr)

	writeDiagHeader(f, tc)

	ctx, cancel := makeContext()
	defer cancel()

	type diagStep struct {
		name   string
		status string
		ok     bool
	}
	var steps []diagStep
	step := func(name string, fn func() (string, error)) bool {
		fmt.Fprintf(f, "\n=== step: %s ===\n", name)
		log.Printf("diag: %s …", name)
		start := time.Now()
		detail, err := fn()
		elapsed := time.Since(start).Round(time.Millisecond)
		if err != nil {
			status := fmt.Sprintf("FAIL after %s: %v", elapsed, err)
			steps = append(steps, diagStep{name, status, false})
			fmt.Fprintf(f, "--- result: %s\n", status)
			log.Printf("diag: %s FAIL: %v", name, err)
			return false
		}
		status := fmt.Sprintf("OK in %s", elapsed)
		if detail != "" {
			status += " (" + detail + ")"
		}
		steps = append(steps, diagStep{name, status, true})
		fmt.Fprintf(f, "--- result: %s\n", status)
		log.Printf("diag: %s OK", name)
		return true
	}

	c := proxy.NewClient(ctx, tc)
	c.Verbose = true
	c.VerboseOut = f

	connected := step("connect & initialize", func() (string, error) {
		if err := c.Connect(); err != nil {
			return "", err
		}
		transport, sessionID, sessionURL := c.SessionInfo()
		detail := "transport=" + transport
		if sessionID != "" {
			detail += ", session=" + sessionID
		}
		if sessionURL != "" {
			detail += ", post-url=" + sessionURL
		}
		return detail, nil
	})

	if connected {
		step("tools/list", func() (string, error) {
			result, err := c.ListTools("")
			if err != nil {
				return "", err
			}
			var parsed struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if jerr := json.Unmarshal(result, &parsed); jerr != nil {
				return fmt.Sprintf("%d bytes result", len(result)), nil
			}
			names := make([]string, 0, len(parsed.Tools))
			for _, tool := range parsed.Tools {
				names = append(names, tool.Name)
			}
			return fmt.Sprintf("%d tools: %s", len(names), strings.Join(names, ", ")), nil
		})

		step("ping", func() (string, error) {
			_, err := c.Ping()
			return "", err
		})

		if toolName != "" {
			step("tools/call "+toolName, func() (string, error) {
				fmt.Fprintf(f, "args: %s\n", argStr)
				result, err := c.CallTool(toolName, json.RawMessage(argStr))
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%d bytes result", len(result)), nil
			})
		}
	}

	failed := 0
	fmt.Fprintf(f, "\n=== summary ===\n")
	fmt.Fprintln(os.Stderr, "\n=== summary ===")
	for _, s := range steps {
		line := fmt.Sprintf("%-40s %s", s.name, s.status)
		fmt.Fprintln(f, line)
		fmt.Fprintln(os.Stderr, line)
		if !s.ok {
			failed++
		}
	}

	absPath, aerr := filepath.Abs(outPath)
	if aerr != nil {
		absPath = outPath
	}
	fmt.Fprintf(os.Stderr, "\nreport written to %s\n", absPath)
	if failed > 0 {
		return fmt.Errorf("%d of %d steps failed (full wire trace in %s)", failed, len(steps), absPath)
	}
	return nil
}

// writeDiagHeader records everything about the environment the run happened
// in, so a report read weeks later still answers "what exactly was tested?".
// Secret header values are redacted — the file is meant to leave the site.
func writeDiagHeader(w io.Writer, tc *proxy.TransportConfig) {
	ver, sha, when, dirty := buildInfo()
	if sha == "" {
		sha = "unknown"
	} else if len(sha) > 12 {
		sha = sha[:12]
	}
	if dirty {
		sha += "-dirty"
	}
	hostname, _ := os.Hostname()

	fmt.Fprintln(w, "=== go-mcp-proxy diagnostic report ===")
	fmt.Fprintf(w, "generated:  %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(w, "proxy:      %s (commit %s", ver, sha)
	if when != "" {
		fmt.Fprintf(w, ", built %s", when)
	}
	fmt.Fprintln(w, ")")
	fmt.Fprintf(w, "os/arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(w, "hostname:   %s\n", hostname)
	fmt.Fprintf(w, "target:     %s\n", tc.ServerURL)
	fmt.Fprintf(w, "transport:  %s\n", tc.Type)
	fmt.Fprintf(w, "insecure:   %v\n", tc.Insecure)

	keys := make([]string, 0, len(tc.Headers))
	for k := range tc.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "header:     %s: %s\n", k, proxy.RedactHeaderValue(k, tc.Headers[k]))
	}

	for _, name := range []string{"MCP_SERVER_URL", "MCP_SSE_URL", "MCP_AUTH_TOKEN", "MCP_HEADERS", "MCP_INSECURE", "MCP_TRANSPORT"} {
		if v, set := os.LookupEnv(name); set {
			switch name {
			case "MCP_AUTH_TOKEN", "MCP_HEADERS":
				fmt.Fprintf(w, "env:        %s is set (%d chars, value redacted)\n", name, len(v))
			default:
				fmt.Fprintf(w, "env:        %s=%s\n", name, v)
			}
		}
	}
}
