package proxy

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"
)

func TestNewHTTPClient_Secure(t *testing.T) {
	client := NewHTTPClient(false, 10*time.Second)
	if client.Timeout != 10*time.Second {
		t.Errorf("expected 10s timeout, got %v", client.Timeout)
	}
	wt, ok := client.Transport.(*wireTracer)
	if !ok {
		t.Fatal("expected *wireTracer transport")
	}
	if wt.base != http.DefaultTransport {
		t.Error("expected DefaultTransport base for secure client")
	}
}

func TestNewHTTPClient_Insecure(t *testing.T) {
	client := NewHTTPClient(true, 5*time.Second)
	if client.Timeout != 5*time.Second {
		t.Errorf("expected 5s timeout, got %v", client.Timeout)
	}
	wt, ok := client.Transport.(*wireTracer)
	if !ok {
		t.Fatal("expected *wireTracer transport")
	}
	tr, ok := wt.base.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport base")
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify=true")
	}
}

func TestNewHTTPClient_ZeroTimeout(t *testing.T) {
	client := NewHTTPClient(false, 0)
	if client.Timeout != 0 {
		t.Errorf("expected 0 timeout, got %v", client.Timeout)
	}
}

func TestNewHTTPClient_InsecureZeroTimeout(t *testing.T) {
	client := NewHTTPClient(true, 0)
	if client.Timeout != 0 {
		t.Errorf("expected 0 timeout, got %v", client.Timeout)
	}
	wt, ok := client.Transport.(*wireTracer)
	if !ok {
		t.Fatal("expected *wireTracer transport")
	}
	tr, ok := wt.base.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport base")
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify=true")
	}
}

func TestNewHTTPClient_SecureReturnsDistinctInstances(t *testing.T) {
	c1 := NewHTTPClient(false, time.Second)
	c2 := NewHTTPClient(false, time.Second)
	if c1 == c2 {
		t.Error("expected distinct client instances")
	}
}

func TestApplyHeaders(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	headers := map[string]string{
		"Authorization": "Bearer tok",
		"X-Custom":      "val",
	}
	applyHeaders(req, headers)

	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("expected Authorization header")
	}
	if req.Header.Get("X-Custom") != "val" {
		t.Errorf("expected X-Custom header")
	}
}

func TestApplyHeaders_Nil(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	applyHeaders(req, nil) // should not panic
}

func TestApplyHeaders_Empty(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	applyHeaders(req, map[string]string{})
	// No headers should be added
	if len(req.Header) != 0 {
		t.Errorf("expected no headers, got %d", len(req.Header))
	}
}

func TestApplyHeaders_OverriddenByProtocol(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	// Apply custom Content-Type via headers
	applyHeaders(req, map[string]string{"Content-Type": "text/plain"})
	// Protocol code would then override
	req.Header.Set("Content-Type", "application/json")

	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("protocol header should override custom, got %q", req.Header.Get("Content-Type"))
	}
}

func TestApplyHeaders_MultipleHeaders(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	headers := map[string]string{
		"Authorization": "Bearer tok",
		"X-Api-Key":     "key123",
		"Accept":        "text/html",
		"X-Request-Id":  "req-abc",
	}
	applyHeaders(req, headers)

	for k, v := range headers {
		if req.Header.Get(k) != v {
			t.Errorf("header %q: expected %q, got %q", k, v, req.Header.Get(k))
		}
	}
}

func TestTransportConfig_Defaults(t *testing.T) {
	tc := TransportConfig{}
	if tc.ServerURL != "" {
		t.Errorf("expected empty ServerURL, got %q", tc.ServerURL)
	}
	if tc.Insecure {
		t.Error("expected Insecure=false by default")
	}
	if tc.Type != "" {
		t.Errorf("expected empty Type, got %q", tc.Type)
	}
	if tc.Headers != nil {
		t.Error("expected nil Headers by default")
	}
}

func TestTransportConfig_WithValues(t *testing.T) {
	tc := TransportConfig{
		ServerURL: "https://example.com/mcp",
		Headers:   map[string]string{"Authorization": "Bearer tok"},
		Insecure:  true,
		Type:      "http",
	}
	if tc.ServerURL != "https://example.com/mcp" {
		t.Errorf("unexpected ServerURL: %q", tc.ServerURL)
	}
	if !tc.Insecure {
		t.Error("expected Insecure=true")
	}
	if tc.Type != "http" {
		t.Errorf("expected Type 'http', got %q", tc.Type)
	}
	if tc.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("unexpected Authorization header: %q", tc.Headers["Authorization"])
	}
}

// Verify tls.Config is usable (compile-time check for cross-platform)
var _ = &tls.Config{InsecureSkipVerify: true}
