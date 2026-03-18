package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
		"mcpServers": {
			"test-server": {
				"url": "https://example.com/mcp",
				"type": "http",
				"headers": {"Authorization": "Bearer tok123"},
				"insecure": true
			}
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	sc, ok := cfg.MCPServers["test-server"]
	if !ok {
		t.Fatal("test-server not found in config")
	}
	if sc.URL != "https://example.com/mcp" {
		t.Errorf("unexpected URL: %s", sc.URL)
	}
	if sc.Type != "http" {
		t.Errorf("unexpected type: %s", sc.Type)
	}
	if sc.Headers["Authorization"] != "Bearer tok123" {
		t.Errorf("unexpected Authorization header: %s", sc.Headers["Authorization"])
	}
	if !sc.Insecure {
		t.Error("expected insecure=true")
	}
}

func TestLoadConfig_MultipleServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
		"mcpServers": {
			"server-a": {"url": "https://a.example.com/mcp"},
			"server-b": {"url": "https://b.example.com/sse", "type": "sse"}
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.MCPServers))
	}
	if cfg.MCPServers["server-b"].Type != "sse" {
		t.Errorf("server-b type should be 'sse', got %q", cfg.MCPServers["server-b"].Type)
	}
}

func TestLoadConfig_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for empty mcpServers")
	}
}

func TestLoadConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`not json`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"mcpServers": {"s1": {"url": "https://example.com"}}}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.MCPServers["s1"]
	if sc.Type != "" {
		t.Errorf("expected empty type default, got %q", sc.Type)
	}
	if sc.Insecure {
		t.Error("expected insecure=false by default")
	}
	if sc.Headers != nil && len(sc.Headers) > 0 {
		t.Error("expected nil or empty headers by default")
	}
}

func TestLoadConfig_MultipleHeaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
		"mcpServers": {
			"s1": {
				"url": "https://example.com",
				"headers": {
					"Authorization": "Bearer tok",
					"X-Api-Key": "key123",
					"X-Custom": "val"
				}
			}
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.MCPServers["s1"]
	if len(sc.Headers) != 3 {
		t.Fatalf("expected 3 headers, got %d", len(sc.Headers))
	}
	if sc.Headers["X-Api-Key"] != "key123" {
		t.Errorf("expected X-Api-Key 'key123', got %q", sc.Headers["X-Api-Key"])
	}
}

func TestLoadConfig_MissingMCPServersKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"other": "stuff"}`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error when mcpServers key is missing")
	}
}

func TestHeaderFlag(t *testing.T) {
	var h HeaderFlag
	if err := h.Set("Authorization=Bearer token123"); err != nil {
		t.Fatal(err)
	}
	if err := h.Set("X-Custom=value"); err != nil {
		t.Fatal(err)
	}

	if h["Authorization"] != "Bearer token123" {
		t.Errorf("unexpected Authorization: %s", h["Authorization"])
	}
	if h["X-Custom"] != "value" {
		t.Errorf("unexpected X-Custom: %s", h["X-Custom"])
	}
}

func TestHeaderFlag_InvalidFormat(t *testing.T) {
	var h HeaderFlag
	if err := h.Set("no-equals-sign"); err == nil {
		t.Fatal("expected error for missing =")
	}
}

func TestHeaderFlag_String(t *testing.T) {
	var h HeaderFlag
	// Empty HeaderFlag should return ""
	if s := h.String(); s != "" {
		t.Errorf("expected empty string, got %q", s)
	}

	h.Set("Key=Value")
	s := h.String()
	if s != "Key=Value" {
		t.Errorf("expected 'Key=Value', got %q", s)
	}
}

func TestHeaderFlag_OverwritesSameKey(t *testing.T) {
	var h HeaderFlag
	h.Set("Key=first")
	h.Set("Key=second")
	if h["Key"] != "second" {
		t.Errorf("expected 'second', got %q", h["Key"])
	}
}

func TestHeaderFlag_TrimSpaces(t *testing.T) {
	var h HeaderFlag
	h.Set("  Key  =  Value  ")
	if h["Key"] != "Value" {
		t.Errorf("expected trimmed 'Value', got %q", h["Key"])
	}
}

func TestHeaderFlag_ValueWithEquals(t *testing.T) {
	var h HeaderFlag
	// Value containing '=' should not be split further
	if err := h.Set("Authorization=Bearer abc=def=="); err != nil {
		t.Fatal(err)
	}
	if h["Authorization"] != "Bearer abc=def==" {
		t.Errorf("expected 'Bearer abc=def==', got %q", h["Authorization"])
	}
}

func TestHeaderFlag_EmptyValue(t *testing.T) {
	var h HeaderFlag
	if err := h.Set("Key="); err != nil {
		t.Fatal(err)
	}
	if h["Key"] != "" {
		t.Errorf("expected empty value, got %q", h["Key"])
	}
}
