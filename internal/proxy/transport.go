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

// NewHTTPClient creates an *http.Client, optionally skipping TLS certificate verification.
func NewHTTPClient(insecure bool, timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return client
}

// applyHeaders sets custom headers on a request. These are applied before
// transport-specific headers (Content-Type, Accept, Mcp-Session-Id) so that
// protocol headers take precedence.
func applyHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}
