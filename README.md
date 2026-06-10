# go-mcp-proxy

A stdio-to-HTTP proxy for MCP (Model Context Protocol) servers. It lets stdio-based MCP clients like Claude Code connect to remote MCP servers over HTTP or SSE, with support for custom headers, TLS options, and authentication.

For a hands-on user guide (Windows quoting, debugging workflows, REPL usage), see [docs/instructions.md](docs/instructions.md).

## Build

Requires Go 1.23+.

```bash
go build -o go-mcp-proxy ./cmd/go-mcp-proxy/
```

## Usage

```
go-mcp-proxy [flags] [server-url]

Flags:
  -config string    path to JSON config file
  -server string    server name from config (default: first/only server)
  -insecure         skip TLS certificate verification
  -type string      transport: http, sse, auto (default "auto")
  -header value     custom header key=value (repeatable)
```

### Basic

```bash
./go-mcp-proxy https://mcp.example.com/mcp
```

### With authentication and custom headers

```bash
./go-mcp-proxy \
  -header "Authorization=Bearer my-token" \
  -header "X-Api-Key=abc123" \
  https://mcp.example.com/mcp
```

### Self-signed / mismatched TLS certificates

```bash
./go-mcp-proxy -insecure https://10.0.0.5:8443/mcp
```

### Calling tools (subcommand mode)

```bash
# Linux/macOS — single quotes are fine
./go-mcp-proxy call https://mcp.example.com/mcp prtg_get_sensors '{"limit":1}'

# Windows PowerShell — single quotes preserve the JSON literally (same as Unix)
.\mcp-sse-proxy.exe call https://mcp.example.com/mcp prtg_get_sensors '{"limit":1}'

# Windows cmd.exe — single quotes are literal; escape inner double quotes
mcp-sse-proxy.exe call https://mcp.example.com/mcp prtg_get_sensors "{\"limit\":1}"

# Any shell — read JSON from a file (no quoting headaches)
./go-mcp-proxy call https://mcp.example.com/mcp prtg_get_sensors @args.json
```

> **Why this matters on Windows:** PowerShell and `cmd.exe` both strip bare
> double quotes from arguments before the program sees them, so passing
> `{"sensor_id":65635}` ends up as `{sensor_id:65635}` (invalid JSON). The
> proxy detects this case and prints a "did you mean" suggestion, but the
> simplest fix is to wrap the whole payload in single quotes (PowerShell)
> or use `@file` form.

The `@file` form also works inside the `debug` REPL (`call prtg_get_sensors @args.json`).

### Verbose debugging (`-v` / `-vv`)

When the same command behaves differently against two servers, capture both runs
with `-vv` and diff them:

```bash
# -v   print every JSON-RPC frame to stderr
./go-mcp-proxy call -v  https://mcp.example.com/mcp prtg_get_sensor_status '{"sensor_id":65635}'

# -vv  additionally trace the HTTP wire: method, URL, request/response headers
#      (secrets redacted to a prefix + length), status, timing, raw bodies
./go-mcp-proxy call -vv https://mcp.example.com/mcp prtg_get_sensor_status '{"sensor_id":65635}' 2>trace.log
```

The wire trace shows exactly what was sent and received — which URL was hit,
whether the `Authorization` header was applied (and its length, so tokens can
be compared without exposing them), the negotiated `Mcp-Session-Id`, response
status, and the raw body before any parsing.

`-vv` is also available as a global flag for stdio proxy mode
(`./go-mcp-proxy -vv https://…`, trace goes to stderr/the host's MCP log),
and inside the `debug` REPL via the `trace` command (toggles on/off).

### One-shot diagnostic report (`diag`)

`diag` runs the full connectivity check (connect & initialize → tools/list →
ping → optional tool call) and writes **everything** — environment, redacted
configuration, complete wire trace, all JSON-RPC frames, per-step results —
into a single report file you can take with you:

```bash
# Connectivity only
./go-mcp-proxy -header "Authorization=Bearer TOKEN" diag https://mcp.example.com/mcp

# Including a tool call, with a named report file
./go-mcp-proxy -header "Authorization=Bearer TOKEN" diag https://mcp.example.com/mcp \
    prtg_get_sensor_status '{"sensor_id":65635}' -o customer-x.txt
```

Progress and a pass/fail summary print to the terminal; the report file
(default `mcp-diag-<timestamp>.txt`) holds the full detail. Secrets are
redacted to a prefix + length, so the file is safe to take off-site. The exit
code is non-zero if any step failed.

### Using a config file

Create a JSON config:

```json
{
  "mcpServers": {
    "my-server": {
      "url": "https://mcp.example.com/mcp",
      "headers": {
        "Authorization": "Bearer my-token"
      },
      "insecure": false,
      "type": "auto"
    }
  }
}
```

```bash
./go-mcp-proxy -config servers.json -server my-server
```

## Environment Variables

Environment variables are the lowest priority — CLI flags and config file values take precedence.

| Variable | Description |
|---|---|
| `MCP_SERVER_URL` | Server URL (also accepts legacy `MCP_SSE_URL`) |
| `MCP_AUTH_TOKEN` | Sets `Authorization: Bearer <token>` header |
| `MCP_HEADERS` | Comma-separated headers: `Key1=Value1,Key2=Value2` |
| `MCP_INSECURE` | Skip TLS verification (`true` or `1`) |
| `MCP_TRANSPORT` | Transport type: `http` or `sse` |

Examples:

```bash
# URL + bearer token via env
export MCP_SERVER_URL=https://mcp.example.com/mcp
export MCP_AUTH_TOKEN=my-secret-token
./go-mcp-proxy

# Multiple headers via env
export MCP_HEADERS="Authorization=Bearer tok123,X-Api-Key=key456"
./go-mcp-proxy https://mcp.example.com/mcp

# Skip TLS via env
MCP_INSECURE=true ./go-mcp-proxy https://10.0.0.5:8443/mcp
```

## Configuration Priority

Settings are resolved in this order (first wins):

1. CLI flags (`-header`, `-insecure`, positional URL, `-type`)
2. Config file (`-config`)
3. Environment variables (`MCP_SERVER_URL`, `MCP_AUTH_TOKEN`, etc.)

## Transport Modes

The proxy supports two MCP transport protocols:

- **Streamable HTTP** — current MCP standard. Uses `POST` for messages, optional `GET` SSE for server-initiated notifications, session management via `Mcp-Session-Id`.
- **Legacy SSE** — older protocol. Opens a persistent `GET` SSE connection for receiving, `POST` for sending.

By default (`-type auto`), the proxy tries Streamable HTTP first and falls back to Legacy SSE if the server doesn't support it.

## Claude Code Integration

Add the proxy to `.mcp.json` in your project directory or globally at `~/.claude/.mcp.json`.

### Simple — direct URL

```json
{
  "mcpServers": {
    "my-server": {
      "command": "/path/to/go-mcp-proxy",
      "args": ["https://mcp.example.com/sse"]
    }
  }
}
```

### With authentication

```json
{
  "mcpServers": {
    "my-server": {
      "command": "/path/to/go-mcp-proxy",
      "args": [
        "-header", "Authorization=Bearer my-token",
        "https://mcp.example.com/mcp"
      ]
    }
  }
}
```

### With env variables (keeps secrets out of config)

```json
{
  "mcpServers": {
    "my-server": {
      "command": "/path/to/go-mcp-proxy",
      "args": ["https://mcp.example.com/mcp"],
      "env": {
        "MCP_AUTH_TOKEN": "my-secret-token"
      }
    }
  }
}
```

### Self-signed TLS + auth header

```json
{
  "mcpServers": {
    "internal-server": {
      "command": "/path/to/go-mcp-proxy",
      "args": [
        "-insecure",
        "-header", "Authorization=whm root:YOUR_API_TOKEN",
        "https://10.0.0.5:2087/mcp"
      ]
    }
  }
}
```

### Using a config file

```json
{
  "mcpServers": {
    "my-server": {
      "command": "/path/to/go-mcp-proxy",
      "args": [
        "-config", "/path/to/servers.json",
        "-server", "my-server"
      ]
    }
  }
}
```

### Multiple headers via env

```json
{
  "mcpServers": {
    "my-server": {
      "command": "/path/to/go-mcp-proxy",
      "args": [],
      "env": {
        "MCP_SERVER_URL": "https://mcp.example.com/mcp",
        "MCP_HEADERS": "Authorization=Bearer tok123,X-Tenant-Id=acme",
        "MCP_INSECURE": "true"
      }
    }
  }
}
```

### Windows

```json
{
  "mcpServers": {
    "my-server": {
      "command": "C:\\tools\\go-mcp-proxy.exe",
      "args": ["https://mcp.example.com/sse"]
    }
  }
}
```

## How It Works

```
MCP Client (e.g. Claude Code)
    │ stdin (JSON-RPC 2.0)
    ▼
┌──────────────┐
│ go-mcp-proxy │  ← flags / config / env vars
└──────────────┘
    │ HTTPS POST + custom headers
    ▼
Remote MCP Server
    │ JSON response or SSE stream
    ▼
┌──────────────┐
│ go-mcp-proxy │
└──────────────┘
    │ stdout (JSON-RPC 2.0)
    ▼
MCP Client
```

The proxy reads JSON-RPC messages from stdin, forwards them as HTTP POST requests to the remote server, and writes responses back to stdout. All logging goes to stderr so it doesn't interfere with the protocol.

## Project Structure

```
cmd/go-mcp-proxy/main.go    CLI entrypoint
internal/proxy/              Proxy library
  config.go                  Config file + header flag parsing
  jsonrpc.go                 JSON-RPC types and stdin/stdout I/O
  transport.go               HTTP client and header helpers
  sse.go                     SSE parser and URL resolution
  legacy_sse.go              Legacy SSE transport
  streamable.go              Streamable HTTP transport
```

## License

See [LICENSE](LICENSE) file.
