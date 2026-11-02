package hub

import (
	"context"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/store"
)

func TestPollReportsWhatThePeerIsDoing(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventActivity, TurnID: res.TurnID, Activity: "running ls -la"})
	waitFor(t, "the first activity", func() bool {
		p, _ := h.Poll(ctx, res.ConversationID, 0)
		return p.Activity == "running ls -la"
	})

	// The newest label wins: this is "what is it doing now", not a log.
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventActivity, TurnID: res.TurnID, Activity: "Read: /tmp/p/README.md"})
	waitFor(t, "the newer activity", func() bool {
		p, _ := h.Poll(ctx, res.ConversationID, 0)
		return p.Activity == "Read: /tmp/p/README.md"
	})
}

func TestActivityIsNotReportedOnceTheTurnIsOver(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventActivity, TurnID: res.TurnID, Activity: "running go test"})
	waitFor(t, "the activity", func() bool {
		p, _ := h.Poll(ctx, res.ConversationID, 0)
		return p.Activity != ""
	})

	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done"})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)

	p, _ := h.Poll(ctx, res.ConversationID, 0)
	if p.Activity != "" {
		t.Errorf("a finished turn must report no activity, got %q", p.Activity)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
