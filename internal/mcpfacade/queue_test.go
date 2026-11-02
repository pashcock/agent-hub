package mcpfacade

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

func cappedHarness(t *testing.T, limit int) (*mcp.ClientSession, func()) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	fa := fake.New()
	fa.Limit = limit
	h := hub.New(st, map[string]adapter.Adapter{"codex": fa},
		hub.Config{MaxParallelPeerTurns: 10})
	f := New(h, []string{"codex"})
	closeSess, sess := newTestSession(t, f)
	return sess, func() { closeSess(); h.Close(); st.Close() }
}

func TestPeerStartRefusalTellsTheCallerAboutQueue(t *testing.T) {
	sess, done := cappedHarness(t, 1)
	defer done()
	args := map[string]any{"agent": "codex", "task": "x", "cwd": "/tmp/p"}
	callTool(t, sess, "peer_start", args)

	res, err := sess.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "peer_start", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a start beyond capacity must be an error result")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	// The refusal has to be actionable, not just a wall.
	for _, want := range []string{"capacity", "queue=true", "Retry later"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal %q does not mention %q", text, want)
		}
	}
}

func TestPeerStartWithQueueReturnsQueued(t *testing.T) {
	sess, done := cappedHarness(t, 1)
	defer done()
	callTool(t, sess, "peer_start", map[string]any{
		"agent": "codex", "task": "one", "cwd": "/tmp/p"})

	out := callTool(t, sess, "peer_start", map[string]any{
		"agent": "codex", "task": "two", "cwd": "/tmp/p", "queue": true})
	if out["status"] != "queued" {
		t.Fatalf("status = %v, want queued", out["status"])
	}
	if out["conversation_id"] == nil || out["conversation_id"] == "" {
		t.Fatal("a queued start must still return a conversation id")
	}
	if tid, ok := out["turn_id"]; ok && tid != "" {
		t.Errorf("a queued start has no turn yet, got %v", tid)
	}

	poll := callTool(t, sess, "peer_poll",
		map[string]any{"conversation_id": out["conversation_id"]})
	if poll["turn_status"] != "queued" || poll["liveness"] != "queued" {
		t.Errorf("poll = %v / %v, want queued / queued",
			poll["liveness"], poll["turn_status"])
	}
}
