// Package config loads agent-hub configuration from TOML, applying
// defaults for every value the file does not set.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/pashcock/agent-hub/internal/semantic"
)

type Server struct {
	Addr string `toml:"addr"`
}

type Limits struct {
	MaxParallelPeerTurns int `toml:"max_parallel_peer_turns"`
	// StallWarnS is how long a running turn may produce no events before
	// peer_poll flags it. The turn is never killed; a peer that thinks
	// for an hour is legitimate, one that has hung is not, and only the
	// caller can tell them apart.
	StallWarnS int `toml:"stall_warn_s"`
}

type Permissions struct {
	ForwardToCaller bool     `toml:"forward_to_caller"`
	TimeoutS        int      `toml:"timeout_s"`
	OnTimeout       string   `toml:"on_timeout"`
	HumanGated      []string `toml:"human_gated"`
}

type CodexAgent struct {
	IdleKillS int  `toml:"idle_kill_s"`
	Enabled   bool `toml:"enabled"`
}

type ClaudeAgent struct {
	Enabled          bool `toml:"enabled"`
	MaxLiveProcesses int  `toml:"max_live_processes"`
	IdleKillS        int  `toml:"idle_kill_s"`
}

type GrokAgent struct {
	IdleKillS        int  `toml:"idle_kill_s"`
	Enabled          bool `toml:"enabled"`
	MaxLiveProcesses int  `toml:"max_live_processes"`
}

type KimiAgent struct {
	Enabled          bool   `toml:"enabled"`
	Bin              string `toml:"bin"`
	SourceHome       string `toml:"source_home"`
	MaxLiveProcesses int    `toml:"max_live_processes"`
	IdleKillS        int    `toml:"idle_kill_s"`
}

type Agents struct {
	Kimi   KimiAgent   `toml:"kimi"`
	Grok   GrokAgent   `toml:"grok"`
	Codex  CodexAgent  `toml:"codex"`
	Claude ClaudeAgent `toml:"claude"`
}

type Config struct {
	Semantic    semantic.Config `toml:"semantic"`
	Server      Server          `toml:"server"`
	Limits      Limits          `toml:"limits"`
	Permissions Permissions     `toml:"permissions"`
	Agents      Agents          `toml:"agents"`
}

// Defaults returns the configuration used when no file exists.
func Defaults() Config {
	return Config{
		Server: Server{Addr: "127.0.0.1:7777"},
		Limits: Limits{MaxParallelPeerTurns: 3, StallWarnS: 600},
		Permissions: Permissions{
			ForwardToCaller: true,
			TimeoutS:        600,
			OnTimeout:       "deny",
			HumanGated: []string{
				"privilege_escalation",
				"write_outside_cwd",
				"policy_amendment",
			},
		},
		Agents: Agents{
			Kimi:   KimiAgent{MaxLiveProcesses: 4, IdleKillS: 900},
			Grok:   GrokAgent{MaxLiveProcesses: 4, IdleKillS: 900},
			Codex:  CodexAgent{Enabled: true, IdleKillS: 900},
			Claude: ClaudeAgent{Enabled: true, MaxLiveProcesses: 2, IdleKillS: 900},
		},
	}
}

// Load reads path over the defaults. A missing file is not an error.
func Load(path string) (*Config, error) {
	c := Defaults()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Keep defaults.
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	default:
		if err := toml.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if err := validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

func validate(c *Config) error {
	host, _, err := net.SplitHostPort(c.Server.Addr)
	if err != nil {
		return fmt.Errorf("server.addr %q: %w", c.Server.Addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("server.addr %q must be a loopback address", c.Server.Addr)
	}
	if c.Limits.StallWarnS < 0 {
		return fmt.Errorf("limits.stall_warn_s must be >= 0, got %d", c.Limits.StallWarnS)
	}
	if c.Limits.MaxParallelPeerTurns < 1 {
		return fmt.Errorf("limits.max_parallel_peer_turns must be >= 1, got %d",
			c.Limits.MaxParallelPeerTurns)
	}
	if c.Agents.Kimi.IdleKillS < 1 || c.Agents.Kimi.MaxLiveProcesses < 1 {
		return fmt.Errorf("agents.kimi idle_kill_s and max_live_processes must be >= 1")
	}
	if c.Agents.Codex.IdleKillS < 1 {
		return fmt.Errorf("agents.codex.idle_kill_s must be >= 1")
	}
	if c.Agents.Grok.IdleKillS < 1 {
		return fmt.Errorf("agents.grok.idle_kill_s must be >= 1")
	}
	if c.Agents.Grok.MaxLiveProcesses < 1 {
		return fmt.Errorf("agents.grok.max_live_processes must be >= 1")
	}
	switch c.Permissions.OnTimeout {
	case "deny", "accept":
	default:
		return fmt.Errorf("permissions.on_timeout must be deny or accept, got %q",
			c.Permissions.OnTimeout)
	}
	return nil
}

// DefaultPath returns ~/.config/agent-hub/config.toml.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(dir, "agent-hub", "config.toml")
}

// StateDir returns ~/.local/share/agent-hub, creating it if needed.
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "share", "agent-hub")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
