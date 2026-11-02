//go:build integration

package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/mcpfacade"
	"github.com/pashcock/agent-hub/internal/store"
)

type realHarness struct {
	st     *store.Store
	a      *Adapter
	h      *hub.Hub
	client *mcp.ClientSession
	work   string
}

func realHub(t *testing.T, wrap ...func(*Adapter) adapter.Adapter) *realHarness {
	t.Helper()
	if os.Getenv("KIMI_HUB_INTEGRATION") != "1" {
		t.Skip("set KIMI_HUB_INTEGRATION=1; real subscription calls")
	}
	bin, e := ResolveBinary("")
	if e != nil {
		t.Fatal(e)
	}
	home, e := os.UserHomeDir()
	if e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if e = os.Mkdir(work, 0700); e != nil {
		t.Fatal(e)
	}
	a := NewAdapter(Options{Bin: bin, Home: filepath.Join(root, "kimi"), SourceHome: filepath.Join(home, ".kimi-code")})
	st, e := store.Open(filepath.Join(root, "hub.db"))
	if e != nil {
		t.Fatal(e)
	}
	var driver adapter.Adapter = a
	if len(wrap) > 0 {
		driver = wrap[0](a)
	}
	h := hub.New(st, map[string]adapter.Adapter{"kimi": driver}, hub.Config{MaxParallelPeerTurns: 3, HumanGated: []string{"privilege_escalation", "write_outside_cwd", "policy_amendment"}, ApprovalTimeout: time.Minute, OnApprovalTimeout: "deny"})
	f := mcpfacade.New(h, []string{"kimi"})
	ct, sv := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	go f.MCPServer().Run(ctx, sv)
	c, e := mcp.NewClient(&mcp.Implementation{Name: "kimi-integration", Version: "1"}, nil).Connect(ctx, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(); cancel(); h.Close(); a.Close(); st.Close() })
	return &realHarness{st: st, a: a, h: h, client: c, work: work}
}
func (r *realHarness) call(t *testing.T, name string, args any) map[string]any {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
	defer c()
	res, e := r.client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if e != nil {
		t.Fatal(e)
	}
	if res.IsError {
		b, _ := json.Marshal(res.Content)
		t.Fatalf("%s: %s", name, b)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var v map[string]any
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func (r *realHarness) wait(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		p := r.call(t, "peer_poll", map[string]any{"conversation_id": id})
		switch p["turn_status"] {
		case "completed", "failed", "interrupted", "cancelled":
			return p
		case "needs_permission":
			t.Fatalf("unexpected permission: %v", p)
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("turn timeout")
	return nil
}

func TestRealKimiConversation(t *testing.T) {
	r := realHub(t)
	caps := r.call(t, "peer_models", map[string]any{"agent": "kimi", "cwd": r.work})
	t.Logf("models: %v", caps)
	p := r.call(t, "peer_start", map[string]any{"agent": "kimi", "cwd": r.work, "mode": "ask", "model": "kimi-code/k3", "effort": "low", "task": "Do NOT use the superpowers plugin or any of its skills. Do not use tools. Remember codeword CEDAR-7412. Reply exactly OK."})
	id := p["conversation_id"].(string)
	out := r.wait(t, id)
	if out["turn_status"] != "completed" {
		t.Fatalf("first: %v", out)
	}
	t.Logf("first: %v", out)
	events, e := r.st.EventsSince(context.Background(), id, 0, 500)
	if e != nil {
		t.Fatal(e)
	}
	for _, e := range events {
		if e.Kind == "turn_completed" {
			t.Logf("wire %s: %s", e.Kind, e.Raw)
		}
	}
	sid := p["native_session_id"].(string)
	// Force a cold resume without deleting native history.
	s, _ := r.a.get(sid)
	s.mu.Lock()
	proc := s.proc
	s.proc = nil
	s.mu.Unlock()
	proc.close()
	r.call(t, "peer_send", map[string]any{"conversation_id": id, "message": "Do NOT use the superpowers plugin or any of its skills. What codeword did I ask you to remember? Reply only that codeword, no tools."})
	out = r.wait(t, id)
	if out["turn_status"] != "completed" {
		t.Fatalf("resume: %v", out)
	}
	t.Logf("resume: %v", out)
	if out["result"] != "CEDAR-7412" {
		t.Fatalf("lost history: %v", out)
	}
	r.call(t, "peer_close", map[string]any{"conversation_id": id})
}

func TestRealKimiReadOnlyAndModels(t *testing.T) {
	r := realHub(t)
	if e := os.WriteFile(filepath.Join(r.work, "marker.txt"), []byte("ORIGINAL"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, model := range []string{"kimi-code/k3-256k", "kimi-code/kimi-for-coding", "kimi-code/kimi-for-coding-highspeed"} {
		p := r.call(t, "peer_start", map[string]any{"agent": "kimi", "cwd": r.work, "mode": "review", "model": model, "task": "Do NOT use the superpowers plugin or any of its skills. Read marker.txt using Read, then try to overwrite it with CHANGED. If writing tools are unavailable, report that. Do not use subagents."})
		id := p["conversation_id"].(string)
		out := r.wait(t, id)
		if out["turn_status"] != "completed" {
			t.Fatalf("%s: %v", model, out)
		}
		data, e := os.ReadFile(filepath.Join(r.work, "marker.txt"))
		if e != nil || string(data) != "ORIGINAL" {
			t.Fatalf("read-only violated: %s %v", data, e)
		}
		t.Logf("%s: %v", model, out["result"])
		r.call(t, "peer_close", map[string]any{"conversation_id": id})
	}
}

func TestRealKimiWorkspaceWrite(t *testing.T) {
	r := realHub(t)
	p := r.call(t, "peer_start", map[string]any{"agent": "kimi", "cwd": r.work, "mode": "code", "effort": "low", "task": "Do NOT use the superpowers plugin or any of its skills. Use Write to create approved.txt containing APPROVED. Do not use Bash or subagents. Report success."})
	id := p["conversation_id"].(string)
	out := r.wait(t, id)
	data, err := os.ReadFile(filepath.Join(r.work, "approved.txt"))
	if out["turn_status"] != "completed" || err != nil || strings.TrimSpace(string(data)) != "APPROVED" {
		t.Fatalf("write: %v %v", out, err)
	}
	r.call(t, "peer_close", map[string]any{"conversation_id": id})
}

// Test-only standalone driver: production delegated sessions remain isolated.
// Both model invocations cross an MCP hub; no CLI prompt wrapper is used.
func TestRealKimiStandaloneCallsHub(t *testing.T) {
	r := realHub(t)
	facade := mcpfacade.New(r.h, []string{"kimi"})
	const token = "isolated-kimi-integration-token"
	srv := httptest.NewServer(mcpfacade.RequireToken(token, facade.HTTPHandler()))
	defer srv.Close()
	home := t.TempDir()
	o := adapter.StartOptions{Cwd: r.work, Mode: adapter.ModeCode}
	if e := r.a.prepare(o); e != nil {
		t.Fatal(e)
	}
	var c map[string]any
	if _, e := toml.DecodeFile(filepath.Join(r.a.runtimeHome(o), "config.toml"), &c); e != nil {
		t.Fatal(e)
	}
	c["tools"] = map[string]any{"enabled": []string{"mcp__agent-hub__*"}}
	c["permission"] = map[string]any{"rules": []map[string]string{{"decision": "allow", "pattern": "mcp__agent-hub__*"}}}
	f, e := os.OpenFile(filepath.Join(home, "config.toml"), os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = toml.NewEncoder(f).Encode(c); e != nil {
		t.Fatal(e)
	}
	f.Close()
	if e = os.Symlink(filepath.Join(r.a.opts.SourceHome, "credentials"), filepath.Join(home, "credentials")); e != nil {
		t.Fatal(e)
	}
	env := []string{}
	for _, kv := range r.a.environment(o) {
		if !strings.HasPrefix(kv, "KIMI_CODE_HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "KIMI_CODE_HOME="+home)
	var mu sync.Mutex
	var text strings.Builder
	var calls []string
	proc, e := dial(r.a.opts.Bin, r.work, []string{"acp"}, env, func(_ *rpc, m string, p json.RawMessage) {
		if m != "session/update" {
			return
		}
		var v struct {
			Update struct {
				Type    string `json:"sessionUpdate"`
				Content struct{ Text string }
				Title   string
			}
		}
		json.Unmarshal(p, &v)
		mu.Lock()
		defer mu.Unlock()
		if v.Update.Type == "agent_message_chunk" {
			text.WriteString(v.Update.Content.Text)
		}
		if v.Update.Type == "tool_call" {
			calls = append(calls, v.Update.Title)
		}
	}, func(p *rpc, id json.RawMessage, m string, raw json.RawMessage) {
		var req permissionParams
		if m == "session/request_permission" && json.Unmarshal(raw, &req) == nil && strings.Contains(req.ToolCall.Title, "mcp__agent-hub__") {
			for _, o := range req.Options {
				if o.Kind == "allow_once" {
					p.respond(id, map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": o.ID}})
					return
				}
			}
		}
		p.reject(id, "test forbids unexpected client calls")
	})
	if e != nil {
		t.Fatal(e)
	}
	defer proc.close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if e = proc.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, nil); e != nil {
		t.Fatal(e)
	}
	var session sessionResult
	if e = proc.call(ctx, "session/new", map[string]any{"cwd": r.work, "mcpServers": []any{map[string]any{"name": "agent-hub", "type": "http", "url": srv.URL, "headers": []map[string]string{{"name": "Authorization", "value": "Bearer " + token}}}}}, &session); e != nil {
		t.Fatal(e)
	}
	if e = proc.call(ctx, "session/set_config_option", map[string]string{"sessionId": session.SessionID, "configId": "thinking", "value": "low"}, nil); e != nil {
		t.Fatal(e)
	}
	task := fmt.Sprintf("Do NOT use the superpowers plugin or any of its skills. Use agent-hub MCP peer_start to call agent kimi, model kimi-code/k3, effort low, mode ask, cwd %s. Task: 'Do NOT use the superpowers plugin or any of its skills. Do not use tools. Reply exactly HUB-KIMI-OK.' Use peer_poll to collect completion, then peer_close. Report child reply. Do not use other tools.", r.work)
	var result struct {
		Stop string `json:"stopReason"`
	}
	if e = proc.call(ctx, "session/prompt", map[string]any{"sessionId": session.SessionID, "prompt": []map[string]string{{"type": "text", "text": task}}}, &result); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("standalone tools=%v result=%s", calls, text.String())
	if result.Stop != "end_turn" || !strings.Contains(text.String(), "HUB-KIMI-OK") || !strings.Contains(strings.Join(calls, " "), "peer_start") || !strings.Contains(strings.Join(calls, " "), "peer_close") {
		t.Fatal("standalone MCP proof incomplete")
	}
}

func TestRealKimiTerminalAndCancel(t *testing.T) {
	r := realHub(t)
	p := r.call(t, "peer_start", map[string]any{"agent": "kimi", "cwd": r.work, "mode": "code", "effort": "low", "task": "Do NOT use the superpowers plugin or any of its skills. Use Bash to run exactly: printf TERMINAL-OK. Do not use other tools or subagents. Report command output."})
	id := p["conversation_id"].(string)
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		out := r.call(t, "peer_poll", map[string]any{"conversation_id": id})
		switch out["turn_status"] {
		case "needs_permission":
			t.Fatalf("routine command requested approval: %v", out)
		case "completed":
			if !strings.Contains(fmt.Sprint(out["result"]), "TERMINAL-OK") {
				t.Fatalf("terminal proof missing: %v", out)
			}
			r.call(t, "peer_send", map[string]any{"conversation_id": id, "message": "Do NOT use the superpowers plugin or any of its skills. Write a long detailed explanation of sorting algorithms, at least 3000 words. Do not use tools."})
			r.call(t, "peer_cancel", map[string]any{"conversation_id": id})
			cancelled := r.wait(t, id)
			if cancelled["turn_status"] != "interrupted" {
				t.Fatalf("cancel: %v", cancelled)
			}
			r.call(t, "peer_close", map[string]any{"conversation_id": id})
			return
		case "failed", "cancelled":
			t.Fatalf("terminal: %v", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("terminal test timeout")
}

// Checks the exact persistent MCP JSON shape used by standalone Kimi, including
// environment-token lookup. No inference request is sent in this test.
func TestRealKimiMCPConfig(t *testing.T) {
	r := realHub(t)
	const token = "isolated-kimi-config-token"
	f := mcpfacade.New(r.h, []string{"kimi"})
	connected := make(chan struct{}, 1)
	handler := mcpfacade.RequireToken(token, f.HTTPHandler())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "Bearer "+token {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
		handler.ServeHTTP(w, req)
	}))
	defer srv.Close()
	o := adapter.StartOptions{Cwd: r.work, Mode: adapter.ModeCode}
	if e := r.a.prepare(o); e != nil {
		t.Fatal(e)
	}
	home := r.a.runtimeHome(o)
	cfg := map[string]any{"mcpServers": map[string]any{"agent-hub": map[string]string{"url": srv.URL, "bearerTokenEnvVar": "AGENT_HUB_TOKEN"}}}
	data, _ := json.Marshal(cfg)
	if e := os.WriteFile(filepath.Join(home, "mcp.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	env := append(r.a.environment(o), "AGENT_HUB_TOKEN="+token)
	proc, e := dial(r.a.opts.Bin, r.work, []string{"acp"}, env, func(*rpc, string, json.RawMessage) {}, func(p *rpc, id json.RawMessage, _ string, _ json.RawMessage) {
		p.reject(id, "no client requests expected")
	})
	if e != nil {
		t.Fatal(e)
	}
	defer proc.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e = proc.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, nil); e != nil {
		t.Fatal(e)
	}
	var s sessionResult
	if e = proc.call(ctx, "session/new", map[string]any{"cwd": r.work, "mcpServers": []any{}}, &s); e != nil {
		t.Fatal(e)
	}
	select {
	case <-connected:
		t.Log("persistent MCP config authenticated with bearerTokenEnvVar")
	case <-ctx.Done():
		t.Fatal("Kimi did not connect using persistent MCP config")
	}
}
