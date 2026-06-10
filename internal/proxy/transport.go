package proxy

import (
	"crypto/tls"
	"net/http"
	"time"
)

// TransportConfig holds the resolved configuration for connecting to a remote MCP server.
type TransportConfig struct {
	ServerURL string
	Headers   map[string]string
	Insecure  bool
	Type      string // "http", "sse", "auto"
}

// NewHTTPClient creates an *http.Client, optionally skipping TLS certificate
// verification. Every client is wrapped in the wire tracer so SetWireTrace
// can observe all HTTP traffic; the wrapper is a no-op while tracing is off.
func NewHTTPClient(insecure bool, timeout time.Duration) *http.Client {
	var base http.RoundTripper = http.DefaultTransport
	if insecure {
		base = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return &http.Client{Timeout: timeout, Transport: &wireTracer{base: base}}
}

// applyHeaders sets custom headers on a request. These are applied before
// transport-specific headers (Content-Type, Accept, Mcp-Session-Id) so that
// protocol headers take precedence.
func applyHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}
