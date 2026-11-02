package hub

import (
	"context"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/store"
)

func TestPollReportsHowLongATurnHasBeenSilent(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	// A turn that is producing events is not silent.
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventMessageDelta, TurnID: res.TurnID, Text: "thinking"})
	deadline := time.After(5 * time.Second)
	var pr PollResult
	for {
		pr, _ = h.Poll(ctx, res.ConversationID, 0)
		if len(pr.Events) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the delta never reached the store")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if pr.TurnStatus != store.TurnRunning {
		t.Fatalf("turn status = %q, want running", pr.TurnStatus)
	}
	if pr.SilentForS > 5 {
		t.Errorf("SilentForS = %d, want a small number right after an event", pr.SilentForS)
	}
	if pr.Stalled {
		t.Error("a turn that just produced an event must not be reported stalled")
	}
}

func TestStallIsReportedNotFailed(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fa := fake.New()
	h := New(st, map[string]adapter.Adapter{"fake": fa}, Config{
		MaxParallelPeerTurns: 3,
		StallWarn:            80 * time.Millisecond,
	})
	defer h.Close()

	res, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	pr, err := h.Poll(ctx, res.ConversationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Stalled {
		t.Fatalf("a turn silent for longer than StallWarn must be reported stalled: %+v", pr)
	}
	// Reporting, never killing. A legitimate hour-long turn must survive.
	if pr.TurnStatus != store.TurnRunning {
		t.Errorf("turn status = %q; a stall is reported, not enforced", pr.TurnStatus)
	}
	if pr.SilentForS < 0 {
		t.Errorf("SilentForS = %d", pr.SilentForS)
	}
}

func TestACompletedTurnIsNeverStalled(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fa := fake.New()
	h := New(st, map[string]adapter.Adapter{"fake": fa}, Config{
		MaxParallelPeerTurns: 3, StallWarn: 50 * time.Millisecond,
	})
	defer h.Close()

	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done"})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)
	time.Sleep(200 * time.Millisecond)

	pr, _ := h.Poll(ctx, res.ConversationID, 0)
	if pr.Stalled {
		t.Errorf("a finished turn must never be reported stalled: %+v", pr)
	}
}
