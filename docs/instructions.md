# go-mcp-proxy — Usage Instructions

`go-mcp-proxy` (shipped on Windows as `mcp-sse-proxy.exe`) is a command-line
client and stdio proxy for remote MCP servers (Streamable HTTP and legacy SSE).
This guide covers everyday usage and, in particular, how to debug a server
that behaves differently between two systems.

Throughout this document, replace `http://SERVER:8443/mcp` with your server
URL and use the binary name that matches your platform:

| Platform | Binary |
|---|---|
| Linux / macOS | `./go-mcp-proxy` |
| Windows | `.\mcp-sse-proxy.exe` |

## 1. Quick start

```bat
:: List the tools the server offers
mcp-sse-proxy.exe tools http://SERVER:8443/mcp

:: Call a tool (Windows cmd.exe — escape the inner double quotes)
mcp-sse-proxy.exe call http://SERVER:8443/mcp prtg_get_sensor_status "{\"sensor_id\":65635}"

:: Ping the server
mcp-sse-proxy.exe ping http://SERVER:8443/mcp

:: Full diagnostic with report file (see section 4)
mcp-sse-proxy.exe diag http://SERVER:8443/mcp -o report.txt
```

With authentication and a self-signed certificate:

```bat
mcp-sse-proxy.exe -header "Authorization=Bearer YOUR-TOKEN" -insecure tools https://SERVER:8443/mcp
```

Global flags (`-header`, `-insecure`, `-type`, `-vv`) go **before** the
subcommand; per-command flags (`-v`, `-vv`, `--json`, `--full`, `--limit`,
`--lines`, `--cursor`) go **after** it.

## 2. Passing JSON arguments

Shells mangle JSON differently. Pick the form that matches yours:

| Shell | Form |
|---|---|
| Linux/macOS, PowerShell | `'{"sensor_id":65635}'` (single quotes) |
| Windows cmd.exe | `"{\"sensor_id\":65635}"` (escaped double quotes) |
| Any shell | `@args.json` (read the JSON from a file — no quoting at all) |

If the shell strips your quotes anyway (`{sensor_id:65635}`), the proxy
detects it, prints `note: auto-recovered shell-mangled JSON args`, and
proceeds with the corrected JSON. Treat the note as a hint to switch to the
`@file` form.

## 3. Verbosity levels

| Flag | What it shows |
|---|---|
| *(none)* | Formatted results only |
| `-v` | Every JSON-RPC frame (requests, responses, notifications) on stderr |
| `-vv` | Everything from `-v` **plus** the HTTP wire trace |

The wire trace prints one numbered block per HTTP exchange:

```
07:35:09.123 wire #3 → POST http://SERVER:8443/mcp
07:35:09.123 wire #3 →   Authorization: Bearer my-…(redacted, 39 chars total)
07:35:09.123 wire #3 →   Mcp-Session-Id: mcp-session-46e3c5fb-…
07:35:09.123 wire #3 → body: {"jsonrpc":"2.0","id":2,"method":"tools/call",…}
07:35:09.124 wire #3 ⇄ connected to 10.2.0.110:8443 (reused=false)
07:35:09.250 wire #3 ← 200 OK in 126.4ms
07:35:09.250 wire #3 ←   Content-Type: application/json
07:35:09.250 wire #3 ← body: {"jsonrpc":"2.0","id":2,"error":{"code":-32603,…}}
```

It includes: method and exact URL, all request/response headers, connection
target and reuse, TLS version and server certificate CN, status, timing, and
raw bodies (truncated at 4 KB; SSE streams are logged line-by-line as they
arrive). Credential headers (`Authorization`, `X-Api-Key`, `Cookie`, …) are
redacted to a short prefix plus the total length — enough to tell whether two
systems use the same token, without the trace containing a usable secret.

## 4. Debugging "works on system A, fails on system B"

### The one command to run on-site: `diag`

`diag` performs the complete check and writes a single self-contained report
file. When you leave the customer, that file is everything you need:

```bat
:: Connectivity check only
mcp-sse-proxy.exe -header "Authorization=Bearer TOKEN" diag http://SERVER:8443/mcp -o customer-x.txt

:: Including the failing tool call
mcp-sse-proxy.exe -header "Authorization=Bearer TOKEN" diag http://SERVER:8443/mcp prtg_get_sensor_status "{\"sensor_id\":65635}" -o customer-x.txt
```

The report contains, in one file:

- **Environment** — proxy version + build commit, OS/arch, hostname, timestamp
- **Configuration** — target URL, transport, TLS settings, all headers
  (secret values redacted to prefix + length), which `MCP_*` env vars are set
- **Every step with full detail** — connect & initialize (transport
  detection, session ID, server name/version), tools/list (tool names),
  ping, and the optional tool call, each with the complete HTTP wire trace
  and JSON-RPC frames, timing, and an `OK`/`FAIL` result line
- **Summary** — one line per step at the end

Progress prints to the terminal while it runs; without `-o` the file is named
`mcp-diag-<timestamp>.txt` in the current directory. The exit code is
non-zero if any step failed. Credentials never appear in full, so the file is
safe to take off-site and attach to tickets or e-mails.

Run it on both the working and the failing system, take both files, and
compare them (`fc reportA.txt reportB.txt` on Windows, `diff` on Linux).

### Manual alternative: `-vv` traces

If you only need a single command's trace, capture stderr instead:

```bat
:: System A (working)
mcp-sse-proxy.exe -header "Authorization=Bearer TOKEN" call -vv http://SERVER:8443/mcp prtg_get_sensor_status "{\"sensor_id\":65635}" 2> trace-A.log

:: System B (failing)
mcp-sse-proxy.exe -header "Authorization=Bearer TOKEN" call -vv http://SERVER:8443/mcp prtg_get_sensor_status "{\"sensor_id\":65635}" 2> trace-B.log
```

Whichever way you captured the data, check, in order:

1. **URL** — do both `wire #N → POST` lines hit the same host, port, and path?
2. **Auth header** — present on both? Same redacted prefix and length?
   A different length means a different token.
3. **Connection** — does `⇄ connected to …` resolve to the same address?
   A proxy or split DNS can silently route the two systems differently.
4. **TLS** — same version and certificate CN? A TLS-intercepting middlebox
   shows up here.
5. **Status and body** — if both reach the server (HTTP 200) and the error
   arrives as a JSON-RPC `error` object, the transport is fine and the
   difference is on the **server side**.

> **Interpreting `-32603: failed to get sensor: sensor not found`:** this is
> the MCP server itself answering — connection, session, and authentication
> all worked. Sensor/object IDs are instance-specific, so if the two systems
> talk to different PRTG installations, the same ID usually does not exist on
> both. Find the right ID on the failing system first:
>
> ```bat
> mcp-sse-proxy.exe call http://SERVER:8443/mcp prtg_get_sensors "{\"device_name\":\"MYDEVICE\",\"limit\":10}"
> ```

## 5. Interactive REPL

For iterative poking, the REPL keeps one connection open and prints all
frames automatically:

```bat
mcp-sse-proxy.exe -header "Authorization=Bearer TOKEN" debug http://SERVER:8443/mcp
```

Useful REPL commands:

```
tools                          list tools
call <name> {"key":1}          call a tool (or: call <name> @args.json)
trace                          toggle the HTTP wire trace on/off
headers                        show transport, session ID, and configured headers
raw <json-rpc-envelope>        send a raw frame verbatim
help / quit
```

The REPL has **no client-side timeouts**, so a hanging request stays visibly
hanging instead of being masked by a local timeout.

## 6. Proxy mode (Claude Desktop, Cursor, …)

Without a subcommand the binary runs as a stdio MCP proxy. Example
`claude_desktop_config.json` entry:

```json
{
  "mcpServers": {
    "prtg": {
      "command": "C:\\tools\\mcp-sse-proxy.exe",
      "args": ["-header", "Authorization=Bearer YOUR-TOKEN", "https://SERVER:8443/mcp"]
    }
  }
}
```

Add `-vv` to `args` to get the wire trace in the host application's MCP log
files — useful when a server misbehaves only when driven by the real client.

## 7. Configuration reference

Priority: **CLI flags > config file > environment variables**.

| Environment variable | Effect |
|---|---|
| `MCP_SERVER_URL` | Server URL (legacy alias: `MCP_SSE_URL`) |
| `MCP_AUTH_TOKEN` | Sets `Authorization: Bearer <token>` |
| `MCP_HEADERS` | `Key1=Value1,Key2=Value2` |
| `MCP_INSECURE` | `true` / `1` skips TLS verification |
| `MCP_TRANSPORT` | Force `http` or `sse` (default: auto-detect) |

Config file (`-config servers.json -server my-server`):

```json
{
  "mcpServers": {
    "my-server": {
      "url": "https://SERVER:8443/mcp",
      "headers": { "Authorization": "Bearer YOUR-TOKEN" },
      "insecure": false,
      "type": "auto"
    }
  }
}
```

## 8. Version

```bat
mcp-sse-proxy.exe -version
```

Include this output when reporting issues — it contains the build commit.
