package kimi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Copy native MCP definitions without changing transports, headers or env refs.
// Project MCP files remain native Kimi configuration; the tool denylist also
// excludes the hub there. Missing source configuration clears stale definitions.
func (a *Adapter) prepareMCP(home string) error {
	data, err := os.ReadFile(filepath.Join(a.opts.SourceHome, "mcp.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	config := map[string]json.RawMessage{}
	if err == nil {
		if err = json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("kimi: invalid source MCP config: %w", err)
		}
	}
	var servers map[string]json.RawMessage
	if raw := config["mcpServers"]; raw != nil {
		if err = json.Unmarshal(raw, &servers); err != nil {
			return fmt.Errorf("kimi: invalid MCP servers: %w", err)
		}
	}
	if servers == nil {
		servers = map[string]json.RawMessage{}
	}
	for name := range servers {
		if strings.Contains(strings.ReplaceAll(strings.ToLower(name), "_", "-"), "agent-hub") {
			delete(servers, name)
		}
	}
	data, err = json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(home, "mcp-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(home, "mcp.json"))
}
