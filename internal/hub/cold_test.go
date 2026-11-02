package hub

import (
	"context"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/store"
)

func TestSessionColdEventMakesTheConversationCold(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetConversation(ctx, res.ConversationID)

	// Settle the turn, then let the adapter report that it released the
	// process. The hub must stop calling the conversation live.
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done"})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)

	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventSessionCold})

	deadline := time.After(5 * time.Second)
	for {
		got, err := st.GetConversation(ctx, res.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.StatusCold {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("liveness = %q, want cold after the adapter released the process",
				got.Status)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// Poll still answers, from the database, without reviving anything.
	pr, err := h.Poll(ctx, res.ConversationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Liveness != store.StatusCold {
		t.Errorf("Poll reported liveness %q, want cold", pr.Liveness)
	}
	if pr.Result != "done" {
		t.Errorf("Poll lost the result: %q", pr.Result)
	}
}

func TestSessionColdDoesNotDisturbARunningTurn(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	// A well-behaved adapter never evicts a busy session, but the hub
	// must not corrupt its own state if one arrives anyway.
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventSessionCold})
	time.Sleep(200 * time.Millisecond)

	turn, err := st.GetTurn(ctx, res.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != store.TurnRunning {
		t.Errorf("turn status = %q, want it left running", turn.Status)
	}
}
