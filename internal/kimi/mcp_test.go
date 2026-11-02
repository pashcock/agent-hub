package kimi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/pashcock/agent-hub/internal/adapter"
)

func TestNativeProfileAllowsProjectConfiguration(t *testing.T) {
	a := fakeAdapter(t)
	o := adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeCode}
	dir := filepath.Join(o.Cwd, ".kimi-code")
	if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"local.toml": "additional_dir = []\n", "mcp.json": `{"mcpServers":{}}`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.prepare(o); err != nil {
		t.Fatal(err)
	}
	var c struct {
		Mode       string `toml:"default_permission_mode"`
		Tools      struct{ Enabled, Disabled []string }
		Permission struct {
			Guard bool `toml:"dangerous_command_guard"`
			Rules []struct{ Decision, Pattern string }
		}
	}
	if _, err := toml.DecodeFile(filepath.Join(a.runtimeHome(o), "config.toml"), &c); err != nil {
		t.Fatal(err)
	}
	if c.Mode != "yolo" || !c.Permission.Guard {
		t.Fatalf("wrong native mode: %+v", c)
	}
	if !strings.Contains(strings.Join(c.Tools.Enabled, ","), "mcp__*__*") {
		t.Fatal("MCP unavailable")
	}
	for _, r := range c.Permission.Rules {
		if r.Decision == "ask" {
			t.Fatal("blanket approval rule retained")
		}
	}
	if !strings.Contains(strings.Join(a.environment(o), "\n"), "KIMI_CODE_TRUST_WORKSPACE=true") {
		t.Fatal("project configuration not trusted")
	}
	if !strings.Contains(strings.Join(a.environment(o), "\n"), "KIMI_CODE_DANGEROUS_COMMAND_GUARD=true") {
		t.Fatal("guard override missing")
	}
}

func TestMCPImportFiltersHubAndClearsRemovedServers(t *testing.T) {
	a := fakeAdapter(t)
	o := adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeCode}
	source := filepath.Join(a.opts.SourceHome, "mcp.json")
	input := `{"mcpServers":{"agent-hub":{"url":"http://localhost/hub"},"agent_hub":{"command":"hub"},"search":{"command":"search","args":["--stdio"],"env":{"TOKEN":"reference"}},"playwright":{"url":"http://localhost/browser"}}}`
	if err := os.WriteFile(source, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.prepare(o); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]json.RawMessage {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(a.runtimeHome(o), "mcp.json"))
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err = json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		return c.Servers
	}
	servers := read()
	if len(servers) != 2 || servers["search"] == nil || servers["playwright"] == nil {
		t.Fatalf("servers: %v", servers)
	}
	if !strings.Contains(string(servers["search"]), `"TOKEN":"reference"`) {
		t.Fatal("native fields lost")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := a.prepare(o); err != nil {
		t.Fatal(err)
	}
	if len(read()) != 0 {
		t.Fatal("stale servers retained")
	}
}
