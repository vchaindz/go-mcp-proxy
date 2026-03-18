package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config represents a JSON configuration file with named MCP servers.
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig defines the connection parameters for a single MCP server.
type ServerConfig struct {
	URL      string            `json:"url"`
	Type     string            `json:"type"`     // "http", "sse", "" (auto)
	Headers  map[string]string `json:"headers"`
	Insecure bool              `json:"insecure"`
}

// LoadConfig reads and parses a JSON config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if len(cfg.MCPServers) == 0 {
		return nil, fmt.Errorf("config has no mcpServers entries")
	}
	return &cfg, nil
}

// HeaderFlag implements flag.Value for repeatable -header key=value flags.
type HeaderFlag map[string]string

func (h *HeaderFlag) String() string {
	if h == nil {
		return ""
	}
	var parts []string
	for k, v := range *h {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ", ")
}

func (h *HeaderFlag) Set(val string) error {
	k, v, ok := strings.Cut(val, "=")
	if !ok {
		return fmt.Errorf("header must be key=value, got %q", val)
	}
	if *h == nil {
		*h = make(HeaderFlag)
	}
	(*h)[strings.TrimSpace(k)] = strings.TrimSpace(v)
	return nil
}
