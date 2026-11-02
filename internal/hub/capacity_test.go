package hub

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/store"
)

// newCapped builds a hub whose fake agent has room for exactly n live
// sessions, so the capacity gate can be exercised without spawning
// anything real.
func newCapped(t *testing.T, n int) (*Hub, *fake.Adapter, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	fa := fake.New()
	fa.Limit = n
	h := New(st, map[string]adapter.Adapter{"fake": fa}, Config{
		MaxParallelPeerTurns: 10, // out of the way; capacity is what we test
	})
	t.Cleanup(func() { h.Close(); st.Close() })
	return h, fa, st
}

func TestStartRefusesWhenTheAgentIsFullAndNamesTheOption(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newCapped(t, 1)

	if _, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p"}); err != nil {
		t.Fatalf("the first start must fit: %v", err)
	}
	_, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "two", Cwd: "/tmp/p"})
	if err == nil {
		t.Fatal("a start beyond the agent's capacity must be refused")
	}
	// The caller has to be told what to do about it, not just that it failed.
	for _, want := range []string{"queue", "1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestQueuedStartIsAcceptedAndReportsQueued(t *testing.T) {
	ctx := context.Background()
	h, _, st := newCapped(t, 1)
	h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p"})

	res, err := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "two", Cwd: "/tmp/p", Queue: true})
	if err != nil {
		t.Fatalf("an explicitly queued start must be accepted: %v", err)
	}
	if res.ConversationID == "" {
		t.Fatal("a queued start must still return a conversation id")
	}
	if res.Status != store.StatusQueued {
		t.Errorf("status = %q, want %q", res.Status, store.StatusQueued)
	}
	if res.TurnID != "" {
		t.Errorf("a queued start has no turn yet, got turn %q", res.TurnID)
	}

	pr, err := h.Poll(ctx, res.ConversationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Liveness != store.StatusQueued || pr.TurnStatus != store.TurnQueued {
		t.Errorf("poll = liveness %q / turn %q, want queued / queued",
			pr.Liveness, pr.TurnStatus)
	}
	// Nothing was spawned for it.
	c, _ := st.GetConversation(ctx, res.ConversationID)
	if c.NativeSessionID != "" {
		t.Errorf("a queued conversation must hold no session, got %q", c.NativeSessionID)
	}
}

func TestQueuedStartRunsWhenCapacityFrees(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newCapped(t, 1)
	first, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p"})
	queued, err := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "two MANGO", Cwd: "/tmp/p", Queue: true})
	if err != nil {
		t.Fatal(err)
	}

	// Free the slot the way real life does: finish the turn, close the
	// conversation.
	c1, _ := st.GetConversation(ctx, first.ConversationID)
	fa.Emit(c1.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: first.TurnID, Text: "done"})
	waitStatus(t, h, first.ConversationID, store.TurnCompleted)
	if err := h.CloseConversation(ctx, first.ConversationID); err != nil {
		t.Fatal(err)
	}

	// The queued one must start on its own.
	deadline := time.After(10 * time.Second)
	for {
		pr, err := h.Poll(ctx, queued.ConversationID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if pr.Liveness == store.StatusLive && pr.TurnStatus == store.TurnRunning {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the queued conversation never started: liveness %q turn %q",
				pr.Liveness, pr.TurnStatus)
		case <-time.After(20 * time.Millisecond):
		}
	}
	// It must carry the task it was queued with, not a blank one.
	msgs, _ := st.ListMessages(ctx, queued.ConversationID)
	if len(msgs) == 0 || !strings.Contains(msgs[0].Text, "MANGO") {
		t.Fatalf("the queued task was lost: %+v", msgs)
	}
}

func TestCancellingAQueuedStartRemovesItFromTheQueue(t *testing.T) {
	ctx := context.Background()
	h, _, st := newCapped(t, 1)
	h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p"})
	q, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "two", Cwd: "/tmp/p", Queue: true})

	if err := h.Cancel(ctx, q.ConversationID); err != nil {
		t.Fatalf("Cancel on a queued conversation: %v", err)
	}
	c, _ := st.GetConversation(ctx, q.ConversationID)
	if c.Status != store.StatusClosed {
		t.Errorf("liveness = %q, want closed", c.Status)
	}
	left, err := st.ListQueued(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		if e.ConversationID == q.ConversationID {
			t.Fatal("a cancelled conversation must not stay in the queue")
		}
	}
}

func TestAnAgentWithNoLimitIsNeverGated(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newCapped(t, 0) // 0 means unlimited, like Codex
	for i := 0; i < 5; i++ {
		if _, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"}); err != nil {
			t.Fatalf("start %d refused by an unlimited agent: %v", i, err)
		}
	}
}

func TestAQueuedStartSurvivesAHubRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	fa := fake.New()
	fa.Limit = 1
	h := New(st, map[string]adapter.Adapter{"fake": fa}, Config{MaxParallelPeerTurns: 10})
	h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p"})
	q, err := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "queued DURIAN", Cwd: "/tmp/p", Queue: true})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	st.Close()

	// A new hub over the same database must still know about it.
	st2, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	fa2 := fake.New()
	fa2.Limit = 4
	h2 := New(st2, map[string]adapter.Adapter{"fake": fa2}, Config{MaxParallelPeerTurns: 10})
	defer h2.Close()
	if err := h2.Recover(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		pr, err := h2.Poll(ctx, q.ConversationID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if pr.TurnStatus == store.TurnRunning {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the queued start did not survive the restart: %+v", pr)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
