//go:build integration

package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/codex"
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
	if os.Getenv("GROK_HUB_INTEGRATION") != "1" {
		t.Skip("set GROK_HUB_INTEGRATION=1; real subscription calls")
	}
	bin, e := exec.LookPath("grok")
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
	a := NewAdapter(Options{Bin: bin, Home: filepath.Join(root, "grok"), AuthPath: filepath.Join(home, ".grok", "auth.json")})
	st, e := store.Open(filepath.Join(root, "hub.db"))
	if e != nil {
		t.Fatal(e)
	}
	var driver adapter.Adapter = a
	if len(wrap) > 0 {
		driver = wrap[0](a)
	}
	h := hub.New(st, map[string]adapter.Adapter{"grok": driver}, hub.Config{MaxParallelPeerTurns: 3, HumanGated: []string{"privilege_escalation", "write_outside_cwd", "policy_amendment"}, ApprovalTimeout: time.Minute, OnApprovalTimeout: "deny"})
	f := mcpfacade.New(h, []string{"grok"})
	ct, sv := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	go f.MCPServer().Run(ctx, sv)
	c, e := mcp.NewClient(&mcp.Implementation{Name: "grok-integration", Version: "1"}, nil).Connect(ctx, ct, nil)
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
func TestRealGrokCapabilities(t *testing.T) {
	r := realHub(t)
	p := r.call(t, "peer_capabilities", map[string]any{"agent": "grok", "cwd": r.work})
	t.Logf("capabilities: %v", p)
}
func TestRealGrokConversation(t *testing.T) {
	r := realHub(t)
	p := r.call(t, "peer_start", map[string]any{"agent": "grok", "cwd": r.work, "mode": "code", "model": "grok-4.6", "task": "Do NOT use the superpowers plugin or any of its skills. Do not use tools. Remember codeword CEDAR-7412. Reply exactly OK."})
	id := p["conversation_id"].(string)
	sid := p["native_session_id"].(string)
	s, _ := r.a.get(sid)
	s.mu.Lock()
	pid := s.proc.cmd.Process.Pid
	s.mu.Unlock()
	out := r.wait(t, id)
	if out["turn_status"] != "completed" {
		t.Fatalf("first turn: %v", out)
	}
	t.Logf("first turn: %v", out)
	events, _ := r.st.EventsSince(context.Background(), id, 0, 500)
	for _, e := range events {
		if e.Kind == "turn_completed" {
			t.Logf("terminal wire: %s", e.Raw)
		}
	}
	r.call(t, "peer_send", map[string]any{"conversation_id": id, "message": "Do NOT use the superpowers plugin or any of its skills. Do not use tools. Reply with the codeword only."})
	out = r.wait(t, id)
	if !strings.Contains(fmtString(out["result"]), "CEDAR-7412") {
		t.Fatalf("lost context: %v", out)
	}
	s.mu.Lock()
	same := s.proc != nil && s.proc.cmd.Process.Pid == pid
	s.mu.Unlock()
	if !same {
		t.Fatal("second turn replaced process")
	}
	t.Logf("second turn same PID %d: %v", pid, out)
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	proc.close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		cold := s.proc == nil
		s.mu.Unlock()
		if cold {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.call(t, "peer_send", map[string]any{"conversation_id": id, "message": "Do NOT use the superpowers plugin or any of its skills. Do not use tools. Repeat the codeword only."})
	out = r.wait(t, id)
	if !strings.Contains(fmtString(out["result"]), "CEDAR-7412") {
		t.Fatalf("resume lost context: %v", out)
	}
	t.Logf("resumed: %v", out)

}
func fmtString(v any) string { s, _ := v.(string); return s }

func TestRealGrokToolsAndCancel(t *testing.T) {
	r := realHub(t)
	p := r.call(t, "peer_start", map[string]any{"agent": "grok", "cwd": r.work, "mode": "code", "model": "grok-4.6", "task": "Do NOT use the superpowers plugin or any of its skills. In this test directory create probe.txt containing exactly GROK-HUB-OK. Then use Bash to run python3 -c 'print(sum([19,23]))'. Reply with the output. Do not access other projects, network or MCP servers."})
	id := p["conversation_id"].(string)
	out := r.wait(t, id)
	t.Logf("tools: %v", out)
	if out["turn_status"] != "completed" {
		t.Fatal(out)
	}
	b, e := os.ReadFile(filepath.Join(r.work, "probe.txt"))
	if e != nil || strings.TrimSpace(string(b)) != "GROK-HUB-OK" {
		t.Fatalf("file control failed: %q %v", b, e)
	}
	r.call(t, "peer_send", map[string]any{"conversation_id": id, "message": "Do NOT use the superpowers plugin or any of its skills. Use Bash to run sleep 30. Then reply DONE."})
	time.Sleep(time.Second)
	r.call(t, "peer_cancel", map[string]any{"conversation_id": id})
	out = r.wait(t, id)
	t.Logf("cancel: %v", out)
	if out["turn_status"] != "cancelled" && out["turn_status"] != "interrupted" {
		t.Fatalf("cancel failed: %v", out)
	}
}

func TestRealGrokReadOnly(t *testing.T) {
	r := realHub(t)
	p := r.call(t, "peer_start", map[string]any{"agent": "grok", "cwd": r.work, "mode": "review", "model": "grok-4.6", "task": "Do NOT use the superpowers plugin or any of its skills. This is a sandbox enforcement test in a temporary directory. Attempt to create forbidden.txt containing TEST using the file-edit tool. If blocked, report BLOCKED and stop; do not bypass the restriction."})
	id := p["conversation_id"].(string)
	out := r.wait(t, id)
	t.Logf("read-only: %v", out)
	if _, e := os.Stat(filepath.Join(r.work, "forbidden.txt")); !os.IsNotExist(e) {
		t.Fatalf("read-only wrote: %v", e)
	}
	if out["turn_status"] != "completed" && !(out["turn_status"] == "failed" && strings.Contains(fmtString(out["error"]), "read-only permission denied")) {
		t.Fatal(out)
	}
}

func TestRealGrokNativeSubagent(t *testing.T) {
	r := realHub(t)
	p := r.call(t, "peer_start", map[string]any{"agent": "grok", "cwd": r.work, "mode": "code", "model": "grok-4.6", "task": "Do NOT use the superpowers plugin or any of its skills. Test native subagents: use your built-in task tool to launch exactly one general-purpose subagent. Its prompt must include: Do NOT use the superpowers plugin or any of its skills. Compute 19+23 without tools and reply NATIVE-CHILD-42. Wait for its actual result and return it. Do not answer the task yourself. Do not use MCP, Bash, other harnesses, or modify files."})
	id := p["conversation_id"].(string)
	out := r.wait(t, id)
	t.Logf("native subagent: %v", out)
	events, err := r.st.EventsSince(context.Background(), id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if strings.Contains(string(e.Raw), "\"tool_call") && (strings.Contains(string(e.Raw), "subagent_id") || strings.Contains(string(e.Raw), "subagentId")) {
			t.Logf("subagent evidence: %s", e.Raw)
			found = true
		}
	}
	if out["turn_status"] != "completed" || !strings.Contains(fmtString(out["result"]), "NATIVE-CHILD-42") || !found {
		t.Fatalf("native subagent not proven: %v, child evidence=%v", out, found)
	}
}

// standaloneDriver is test-only: create a native Grok orchestrator session
// with an explicitly supplied MCP server. It is driven through an outer MCP
// hub so every model turn still uses peer_start/peer_send. Production delegated
// sessions keep their isolation checks; this fixture is never registered there.
type standaloneDriver struct {
	*Adapter
	servers []any
}

func (d *standaloneDriver) StartSession(ctx context.Context, o adapter.StartOptions) (string, error) {
	s := &session{id: uuid.NewString(), opts: o, q: newQueue(), approvals: map[string]permission{}}
	r, err := d.connect(ctx, s)
	if err != nil {
		s.q.close()
		return "", err
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	err = r.call(ctx, "session/new", map[string]any{"cwd": o.Cwd, "mcpServers": d.servers, "_meta": map[string]any{"sessionId": s.id, "modelId": o.Model, "autoMode": true, "yoloMode": false}}, &result)
	if err != nil || result.SessionID != s.id {
		r.close()
		s.q.close()
		return "", fmt.Errorf("standalone session: %v, id=%s", err, result.SessionID)
	}
	d.mu.Lock()
	d.sessions[s.id] = s
	d.mu.Unlock()
	go d.watch(s, r)
	return s.id, nil
}
func TestRealGrokStandaloneCallsCodex(t *testing.T) {
	var driver *standaloneDriver
	r := realHub(t, func(a *Adapter) adapter.Adapter { driver = &standaloneDriver{Adapter: a}; return driver })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ca, err := codex.Open(ctx, bin, root, "agent-hub", codex.AdapterOptions{DefaultCwd: r.work})
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	st, err := store.Open(filepath.Join(root, "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := hub.New(st, map[string]adapter.Adapter{"codex": ca}, hub.Config{MaxParallelPeerTurns: 1, ApprovalTimeout: time.Minute, OnApprovalTimeout: "deny"})
	defer h.Close()
	const token = "isolated-integration-test-token"
	srv := httptest.NewServer(mcpfacade.RequireToken(token, mcpfacade.New(h, []string{"codex"}).HTTPHandler()))
	defer srv.Close()
	// Close Grok's streaming MCP client before waiting for the HTTP server.
	defer r.a.Close()
	driver.servers = []any{map[string]any{"type": "http", "name": "agent-hub", "url": srv.URL, "headers": []any{map[string]string{"name": "Authorization", "value": "Bearer " + token}}}}
	p := r.call(t, "peer_start", map[string]any{"agent": "grok", "cwd": r.work, "mode": "code", "model": "grok-4.6", "task": "Do NOT use the superpowers plugin or any of its skills. Do not use tools. Reply READY."})
	id := p["conversation_id"].(string)
	if out := r.wait(t, id); out["turn_status"] != "completed" {
		t.Fatal(out)
	}
	sid := p["native_session_id"].(string)
	s, _ := r.a.get(sid)
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	// The production adapter must reject this configuration for a delegated peer.
	if err = verifyIsolation(ctx, proc, sid); err == nil || !strings.Contains(err.Error(), "recursive MCP server") {
		t.Fatalf("expected recursive MCP isolation rejection, got %v", err)
	}
	message := fmt.Sprintf("Do NOT use the superpowers plugin or any of its skills. You are now testing standalone Grok as an orchestrator with the explicitly attached agent-hub MCP. Discover its peer_start and peer_poll tools, using search_tool/use_tool if necessary. Start exactly one codex conversation with cwd=%q, mode=ask, model=gpt-5.6-sol, effort=low. Its task must be: Do NOT use the superpowers plugin or any of its skills. Do not use tools. Reply exactly CODEX-PEER-42. Poll that SAME conversation until completed, then return the actual result and conversation_id. Do not invoke Claude or any other agent, native subagents, Bash, or file tools. Do not invent results.", r.work)
	r.call(t, "peer_send", map[string]any{"conversation_id": id, "message": message})
	out := r.wait(t, id)
	t.Logf("standalone Grok: %v", out)
	convs, err := st.ListConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || convs[0].PeerAgent != "codex" {
		t.Fatalf("expected one actual Codex conversation, got %+v", convs)
	}
	turns, err := st.ListTurns(ctx, convs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Status != "completed" {
		t.Fatalf("Codex did not complete: %+v", turns)
	}
	if out["turn_status"] != "completed" || !strings.Contains(fmtString(out["result"]), "CODEX-PEER-42") || !strings.Contains(fmtString(out["result"]), convs[0].ID) {
		t.Fatal(out)
	}
	t.Logf("confirmed target hub: codex conversation=%s turn=%s status=%s", convs[0].ID, turns[0].ID, turns[0].Status)
}
