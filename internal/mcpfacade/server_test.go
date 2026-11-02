package mcpfacade

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

type harness struct {
	sess *mcp.ClientSession
	fa   *fake.Adapter
	st   *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	fa := fake.New()
	h := hub.New(st, map[string]adapter.Adapter{"codex": fa}, hub.Config{
		MaxParallelPeerTurns: 3,
		HumanGated:           []string{"privilege_escalation", "write_outside_cwd", "policy_amendment"},
	})
	f := New(h, []string{"codex"})

	closeSess, sess := newTestSession(t, f)
	t.Cleanup(func() { closeSess(); h.Close(); st.Close() })
	return &harness{sess: sess, fa: fa, st: st}
}

func newTestSession(t *testing.T, f *Facade) (func(), *mcp.ClientSession) {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	go f.MCPServer().Run(context.Background(), st)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return func() { sess.Close() }, sess
}

func callTool(t *testing.T, sess *mcp.ClientSession, name string, args any) map[string]any {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tool %s returned an error result: %+v", name, res.Content)
	}
	var out map[string]any
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s output: %v", name, err)
	}
	return out
}

func call(t *testing.T, h *harness, name string, args any) map[string]any {
	t.Helper()
	res, err := h.sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: name, Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tool %s returned an error result: %+v", name, res.Content)
	}
	var out map[string]any
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s output: %v", name, err)
	}
	return out
}

func TestAllToolsAreRegistered(t *testing.T) {
	h := newHarness(t)
	res, err := h.sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	want := []string{
		"peer_list", "peer_capabilities", "peer_models", "peer_start",
		"peer_send", "peer_poll", "peer_ask", "peer_steer", "peer_cancel",
		"peer_approve", "peer_transcript", "peer_close",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q is not registered; have %v", name, got)
		}
	}
}

func TestPeerStartReturnsImmediately(t *testing.T) {
	h := newHarness(t)
	started := time.Now()
	out := call(t, h, "peer_start", map[string]any{
		"agent": "codex", "task": "do the thing", "cwd": "/tmp/p", "mode": "code",
	})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("peer_start took %v; it must not block on the peer", elapsed)
	}
	if out["conversation_id"] == "" || out["conversation_id"] == nil {
		t.Fatalf("peer_start output = %+v", out)
	}
	if out["status"] != "running" {
		t.Errorf("status = %v, want running", out["status"])
	}
}

func TestPeerSendThenPollDeliversTheResult(t *testing.T) {
	h := newHarness(t)
	start := call(t, h, "peer_start", map[string]any{
		"agent": "codex", "task": "first", "cwd": "/tmp/p",
	})
	convID := start["conversation_id"].(string)

	conv, err := h.st.GetConversation(context.Background(), convID)
	if err != nil {
		t.Fatal(err)
	}
	h.fa.Emit(conv.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: start["turn_id"].(string), Text: "answer one"})

	var poll map[string]any
	deadline := time.After(5 * time.Second)
	for {
		poll = call(t, h, "peer_poll", map[string]any{"conversation_id": convID})
		if poll["turn_status"] == "completed" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("turn never completed: %+v", poll)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if poll["result"] != "answer one" {
		t.Errorf("result = %v, want %q", poll["result"], "answer one")
	}

	send := call(t, h, "peer_send", map[string]any{
		"conversation_id": convID, "message": "second",
	})
	if send["turn_id"] == start["turn_id"] {
		t.Error("peer_send must start a new turn")
	}
	if send["status"] != "running" {
		t.Errorf("status = %v, want running", send["status"])
	}
}

func TestPeerListNamesConfiguredAgents(t *testing.T) {
	h := newHarness(t)
	out := call(t, h, "peer_list", map[string]any{})
	agents, ok := out["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("agents = %v", out["agents"])
	}
	first := agents[0].(map[string]any)
	if first["name"] != "codex" {
		t.Errorf("agent name = %v, want codex", first["name"])
	}
}

func TestPeerApproveRefusesHumanGatedRequest(t *testing.T) {
	h := newHarness(t)
	start := call(t, h, "peer_start", map[string]any{
		"agent": "codex", "task": "x", "cwd": "/tmp/p",
	})
	convID := start["conversation_id"].(string)
	conv, _ := h.st.GetConversation(context.Background(), convID)

	h.fa.Emit(conv.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: start["turn_id"].(string),
		Approval: &adapter.ApprovalRequest{
			ID: "ap_x", Class: adapter.ClassCommand,
			Command: "sudo rm -rf /tmp/x", Cwd: "/tmp/p"},
	})

	deadline := time.After(5 * time.Second)
	for {
		poll := call(t, h, "peer_poll", map[string]any{"conversation_id": convID})
		if poll["turn_status"] == "needs_permission" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("approval never surfaced through peer_poll")
		case <-time.After(20 * time.Millisecond):
		}
	}

	res, err := h.sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "peer_approve",
		Arguments: map[string]any{
			"conversation_id": convID, "request_id": "ap_x", "decision": "accept"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("peer_approve must refuse a human-gated request")
	}
}

func TestUnknownConversationIsAToolErrorNotATransportError(t *testing.T) {
	h := newHarness(t)
	res, err := h.sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "peer_poll", Arguments: map[string]any{"conversation_id": "nope"},
	})
	if err != nil {
		t.Fatalf("a bad argument must not break the transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("peer_poll on an unknown conversation must return an error result")
	}
}

func TestPollAccountingWire(t *testing.T) {
	u := store.Usage{InputTokens: 20, CachedInput: 80, TotalTokens: 110}
	ms := int64(1500)
	out := toPollOut(hub.PollResult{Agent: "claude", Usage: &u, Timings: &store.Timings{TotalMS: 500},
		Conversation: &hub.ConversationAccounting{Usage: hub.ConsumptionOf("claude", u), AccountingCoverage: store.AccountingCoverage{Turns: 2, ExecutionMS: &ms}}})
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	usage := v["usage"].(map[string]any)
	if usage["cached_input_tokens"] != float64(80) || usage["cache_hit_percent"] != float64(80) || usage["input_including_cache"] != float64(100) {
		t.Fatalf("usage: %s", raw)
	}
	conv := v["conversation"].(map[string]any)
	if conv["turns"] != float64(2) || conv["execution_ms"] != float64(1500) {
		t.Fatalf("conversation: %s", raw)
	}
}
