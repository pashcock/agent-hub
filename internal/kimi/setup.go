package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pashcock/agent-hub/internal/adapter"
)

// ResolveBinary also works before a newly installed Kimi enters the login PATH.
func ResolveBinary(configured string) (string, error) {
	if configured != "" {
		return exec.LookPath(configured)
	}
	if p, e := exec.LookPath("kimi"); e == nil {
		return p, nil
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return exec.LookPath(filepath.Join(home, ".kimi-code", "bin", "kimi"))
}

func (a *Adapter) runtimeHome(o adapter.StartOptions) string {
	mode := "code"
	if o.Mode.ReadOnly() {
		mode = "read-only"
	}
	return filepath.Join(a.opts.Home, mode)
}

func (a *Adapter) sourceConfig() (map[string]any, error) {
	var c map[string]any
	_, e := toml.DecodeFile(filepath.Join(a.opts.SourceHome, "config.toml"), &c)
	return c, e
}

// Keep credentials and model settings in a private home; allow native project configuration.
func (a *Adapter) prepare(o adapter.StartOptions) error {
	a.prepareMu.Lock()
	defer a.prepareMu.Unlock()
	if !filepath.IsAbs(a.opts.Home) || !filepath.IsAbs(a.opts.SourceHome) || filepath.Clean(a.opts.Home) == filepath.Clean(a.opts.SourceHome) {
		return fmt.Errorf("kimi: distinct absolute runtime and source homes are required")
	}
	source, e := a.sourceConfig()
	if e != nil {
		return e
	}
	c := map[string]any{}
	for _, k := range []string{"default_model", "providers", "models", "thinking", "services"} {
		if v, ok := source[k]; ok {
			c[k] = v
		}
	}
	c["default_permission_mode"] = "manual"
	if !o.Mode.ReadOnly() {
		c["default_permission_mode"] = "yolo"
	}
	c["default_plan_mode"] = false
	c["telemetry"] = false
	c["auto_session_title"] = false
	c["builtin_product_skills"] = false
	c["merge_all_available_skills"] = false
	c["background"] = map[string]any{"keep_alive_on_exit": false}
	c["watch"] = map[string]any{"enabled": false}
	disabled := []string{"mcp__agent-hub__*", "mcp__agent_hub__*", "EnterPlanMode", "ExitPlanMode", "CronCreate"}
	toolConfig := map[string]any{"disabled": disabled}
	toolConfig["enabled"] = []string{"Read", "Glob", "Grep", "Bash", "Write", "Edit", "WebSearch", "FetchURL", "select_tools", "mcp__*__*"}
	if o.Mode.ReadOnly() {
		toolConfig["enabled"] = []string{"Read", "Glob", "Grep", "WebSearch", "FetchURL", "select_tools", "mcp__*__*"}
	}
	c["tools"] = toolConfig
	c["permission"] = map[string]any{"dangerous_command_guard": true, "rules": []map[string]any{
		{"decision": "deny", "pattern": "mcp__agent-hub__*"},
		{"decision": "deny", "pattern": "mcp__agent_hub__*"},
	}}
	if o.Mode.ReadOnly() {
		permission := c["permission"].(map[string]any)
		permission["rules"] = append(permission["rules"].([]map[string]any), map[string]any{"decision": "allow", "pattern": "mcp__*__*"})
	}
	home := a.runtimeHome(o)
	if e = os.MkdirAll(filepath.Join(home, "empty-skills"), 0700); e != nil {
		return e
	}
	if e = a.prepareMCP(home); e != nil {
		return e
	}
	credentials := filepath.Join(home, "credentials")
	target := filepath.Join(a.opts.SourceHome, "credentials")
	if link, err := os.Readlink(credentials); os.IsNotExist(err) {
		if e = os.Symlink(target, credentials); e != nil {
			return e
		}
	} else if err != nil || link != target {
		return fmt.Errorf("kimi: unexpected credentials path %s", credentials)
	}
	for _, name := range []string{"device_id", "region"} {
		data, err := os.ReadFile(filepath.Join(a.opts.SourceHome, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if e = os.WriteFile(filepath.Join(home, name), data, 0600); e != nil {
			return e
		}
	}
	var buf bytes.Buffer
	if e = toml.NewEncoder(&buf).Encode(c); e != nil {
		return e
	}
	f, e := os.CreateTemp(home, "config-*.toml")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(buf.Bytes()); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), filepath.Join(home, "config.toml"))
}

func (a *Adapter) environment(o adapter.StartOptions) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "AGENT_HUB_TOKEN" || strings.HasPrefix(k, "KIMI_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "KIMI_CODE_HOME="+a.runtimeHome(o), "KIMI_CODE_NO_AUTO_UPDATE=1", "KIMI_DISABLE_TELEMETRY=1", "KIMI_CODE_WATCH=false", "KIMI_CODE_BACKGROUND_KEEP_ALIVE_ON_EXIT=false", "KIMI_DISABLE_CRON=1", "KIMI_CODE_DANGEROUS_COMMAND_GUARD=true", "KIMI_CODE_TRUST_WORKSPACE=true")
}

type configOption struct {
	ID      string `json:"id"`
	Current string `json:"currentValue"`
	Options []struct {
		Value string `json:"value"`
	} `json:"options"`
}
type sessionResult struct {
	SessionID string         `json:"sessionId"`
	Config    []configOption `json:"configOptions"`
}

func selected(opts []configOption, id string) string {
	for _, o := range opts {
		if o.ID == id {
			return o.Current
		}
	}
	return ""
}
func (a *Adapter) connect(ctx context.Context, s *session) (*rpc, error) {
	if e := a.prepare(s.opts); e != nil {
		return nil, e
	}
	args := []string{"--skills-dir", filepath.Join(a.runtimeHome(s.opts), "empty-skills"), "acp"}
	r, e := dial(a.opts.Bin, s.opts.Cwd, args, a.environment(s.opts), func(r *rpc, m string, p json.RawMessage) { a.notification(s, r, m, p) }, func(r *rpc, id json.RawMessage, m string, p json.RawMessage) { a.request(s, r, id, m, p) })
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		r.close()
		return nil, adapter.ErrSessionGone
	}
	s.proc = r
	s.mu.Unlock()
	var result struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	e = r.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientInfo": map[string]string{"name": "agent-hub", "version": "1"}, "clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": true, "writeTextFile": true}, "terminal": true}}, &result)
	if e == nil && (result.ProtocolVersion != 1 || !result.AgentCapabilities.LoadSession) {
		e = fmt.Errorf("kimi: ACP v1 and loadSession required")
	}
	if e != nil {
		s.mu.Lock()
		s.proc = nil
		s.mu.Unlock()
		r.close()
		return nil, e
	}
	return r, nil
}

func (a *Adapter) attach(ctx context.Context, s *session, resume bool) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	s.mu.Lock()
	s.lastUsed = time.Time{}
	s.mu.Unlock()
	r, e := a.connect(ctx, s)
	if e != nil {
		return e
	}
	fail := func(err error) error { s.mu.Lock(); s.proc = nil; s.mu.Unlock(); r.close(); return err }
	method := "session/new"
	params := map[string]any{"cwd": s.opts.Cwd, "mcpServers": []any{}}
	if resume {
		method = "session/load"
		params["sessionId"] = s.id
	}
	var result sessionResult
	if e = r.call(ctx, method, params, &result); e != nil {
		return fail(e)
	}
	if !resume {
		if result.SessionID == "" {
			return fail(fmt.Errorf("kimi: missing session id"))
		}
		s.mu.Lock()
		s.id = result.SessionID
		s.mu.Unlock()
	}
	mode := "default"
	if !s.opts.Mode.ReadOnly() {
		mode = "yolo"
	}
	for _, setting := range []struct{ id, value string }{{"model", s.opts.Model}, {"thinking", s.opts.Effort}, {"mode", mode}} {
		if setting.value == "" {
			continue
		}
		var updated sessionResult
		if e = r.call(ctx, "session/set_config_option", map[string]string{"sessionId": s.id, "configId": setting.id, "value": setting.value}, &updated); e != nil {
			return fail(e)
		}
		if selected(updated.Config, setting.id) != setting.value {
			return fail(fmt.Errorf("kimi: did not confirm %s=%q", setting.id, setting.value))
		}
	}
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
	return nil
}

// Native configuration holds model aliases and per-model efforts. Verify that
// they are actually advertised by this CLI through a no-prompt ACP session.
func (a *Adapter) Capabilities(ctx context.Context, cwd string) (adapter.Capabilities, error) {
	if e := a.reserve(); e != nil {
		return adapter.Capabilities{}, e
	}
	defer a.unreserve()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if e := os.MkdirAll(a.opts.Home, 0700); e != nil {
		return adapter.Capabilities{}, e
	}
	s := &session{opts: adapter.StartOptions{Cwd: a.opts.Home, Mode: adapter.ModeAsk}, q: newQueue(), approvals: map[string]permission{}}
	defer s.q.close()
	r, e := a.connect(ctx, s)
	if e != nil {
		return adapter.Capabilities{}, e
	}
	defer r.close()
	var result sessionResult
	if e = r.call(ctx, "session/new", map[string]any{"cwd": a.opts.Home, "mcpServers": []any{}}, &result); e != nil {
		return adapter.Capabilities{}, e
	}
	defer r.call(ctx, "session/delete", map[string]string{"sessionId": result.SessionID}, nil)
	source, e := a.sourceConfig()
	if e != nil {
		return adapter.Capabilities{}, e
	}
	data, _ := json.Marshal(source["models"])
	var configured map[string]struct {
		Model     string   `json:"model"`
		Efforts   []string `json:"support_efforts"`
		Default   string   `json:"default_effort"`
		Overrides struct {
			Efforts *[]string `json:"support_efforts"`
			Default *string   `json:"default_effort"`
		} `json:"overrides"`
	}
	if e = json.Unmarshal(data, &configured); e != nil {
		return adapter.Capabilities{}, e
	}
	caps := adapter.Capabilities{}
	for _, o := range result.Config {
		if o.ID != "model" {
			continue
		}
		for _, v := range o.Options {
			m := configured[v.Value]
			if m.Overrides.Efforts != nil {
				m.Efforts = *m.Overrides.Efforts
			}
			if m.Overrides.Default != nil {
				m.Default = *m.Overrides.Default
			}
			caps.Models = append(caps.Models, adapter.Model{ID: v.Value, ResolvedModel: m.Model, Efforts: m.Efforts, DefaultEffort: m.Default})
		}
	}
	if len(caps.Models) == 0 {
		return caps, fmt.Errorf("kimi: ACP did not advertise a model catalog")
	}
	sort.Slice(caps.Models, func(i, j int) bool { return caps.Models[i].ID < caps.Models[j].ID })
	return caps, nil
}
