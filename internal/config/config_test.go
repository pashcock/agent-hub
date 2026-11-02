package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsWhenFileMissing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load with missing file must succeed, got %v", err)
	}
	if c.Server.Addr != "127.0.0.1:7777" {
		t.Errorf("Server.Addr = %q, want 127.0.0.1:7777", c.Server.Addr)
	}
	if c.Limits.MaxParallelPeerTurns != 3 {
		t.Errorf("MaxParallelPeerTurns = %d, want 3", c.Limits.MaxParallelPeerTurns)
	}
	if c.Limits.StallWarnS != 600 {
		t.Errorf("StallWarnS = %d, want 600", c.Limits.StallWarnS)
	}
	if c.Permissions.TimeoutS != 600 {
		t.Errorf("TimeoutS = %d, want 600", c.Permissions.TimeoutS)
	}
	if c.Permissions.OnTimeout != "deny" {
		t.Errorf("OnTimeout = %q, want deny", c.Permissions.OnTimeout)
	}
	if !c.Agents.Codex.Enabled {
		t.Error("codex must be enabled by default")
	}
	want := []string{"privilege_escalation", "write_outside_cwd", "policy_amendment"}
	if len(c.Permissions.HumanGated) != len(want) {
		t.Fatalf("HumanGated = %v, want %v", c.Permissions.HumanGated, want)
	}
	for i := range want {
		if c.Permissions.HumanGated[i] != want[i] {
			t.Errorf("HumanGated[%d] = %q, want %q", i, c.Permissions.HumanGated[i], want[i])
		}
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	body := `
[server]
addr = "127.0.0.1:9999"

[limits]
max_parallel_peer_turns = 7

[agents.codex]
enabled = false
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Addr != "127.0.0.1:9999" {
		t.Errorf("Server.Addr = %q, want 127.0.0.1:9999", c.Server.Addr)
	}
	if c.Limits.MaxParallelPeerTurns != 7 {
		t.Errorf("MaxParallelPeerTurns = %d, want 7", c.Limits.MaxParallelPeerTurns)
	}
	if c.Agents.Codex.Enabled {
		t.Error("codex must be disabled by the file")
	}
	// Unset values must keep their defaults.
	if c.Permissions.TimeoutS != 600 {
		t.Errorf("TimeoutS = %d, want 600", c.Permissions.TimeoutS)
	}
}

func TestRejectsNonLoopbackAddr(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[server]\naddr = \"0.0.0.0:7777\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("Load must reject a non-loopback listen address")
	}
}

func TestGrokIsOptInAndHasCapacity(t *testing.T) {
	c := Defaults()
	if c.Agents.Grok.Enabled || c.Agents.Grok.MaxLiveProcesses != 4 {
		t.Fatal(c.Agents.Grok)
	}
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[agents.grok]\nenabled = true\nmax_live_processes = 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Agents.Grok.Enabled || got.Agents.Grok.MaxLiveProcesses != 2 {
		t.Fatal(got.Agents.Grok)
	}
}

func TestKimiOptInAndOverrides(t *testing.T) {
	c := Defaults()
	if c.Agents.Kimi.Enabled || c.Agents.Kimi.MaxLiveProcesses != 4 || c.Agents.Kimi.IdleKillS != 900 {
		t.Fatal(c.Agents.Kimi)
	}
	p := filepath.Join(t.TempDir(), "config.toml")
	for _, capacity := range []string{"2", "0"} {
		os.WriteFile(p, []byte("[agents.kimi]\nenabled=true\nbin='/opt/kimi'\nsource_home='/account/kimi'\nmax_live_processes="+capacity+"\n"), 0600)
		got, e := Load(p)
		if capacity == "0" {
			if e == nil {
				t.Fatal("zero capacity accepted")
			}
			continue
		}
		if e != nil || got.Agents.Kimi.Bin != "/opt/kimi" || !got.Agents.Kimi.Enabled {
			t.Fatalf("%+v %v", got, e)
		}
	}
}
