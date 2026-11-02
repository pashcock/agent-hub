package hub

import (
	"context"
	"testing"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/store"
)

func TestUsageIsRecordedAndReportedOnlyWhenTheTurnEnds(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	// A measurement arrives mid-turn, as Codex sends it.
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTokenUsage, TurnID: res.TurnID,
		Usage: &adapter.Usage{InputTokens: 1000, OutputTokens: 200,
			TotalTokens: 1200, Model: "gpt-5.6-luna"}})

	// While the turn runs, the caller must not be given usage: it is
	// noise in a conversation between agents, and it is not final.
	deadline := 0
	var pr PollResult
	for deadline < 200 {
		pr, _ = h.Poll(ctx, res.ConversationID, 0)
		if u, _ := st.TurnUsage(ctx, res.TurnID); u.TotalTokens == 1200 {
			break
		}
		deadline++
	}
	if pr.Usage != nil {
		t.Errorf("a running turn must not report usage, got %+v", pr.Usage)
	}

	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done"})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)

	pr, err := h.Poll(ctx, res.ConversationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Usage == nil {
		t.Fatal("a finished turn must report what it consumed")
	}
	if pr.Usage.TotalTokens != 1200 || pr.Usage.Model != "gpt-5.6-luna" {
		t.Errorf("usage = %+v", pr.Usage)
	}
}

func TestUsageOnTheTerminalEventItselfIsRecorded(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	// Claude reports usage on the result line, not separately.
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done",
		Usage: &adapter.Usage{InputTokens: 2, OutputTokens: 455,
			CacheWrite: 35057, TotalTokens: 35514, CostUSD: 0.144782,
			Model: "claude-sonnet-5"}})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)

	pr, _ := h.Poll(ctx, res.ConversationID, 0)
	if pr.Usage == nil || pr.Usage.CostUSD == 0 {
		t.Fatalf("cost must survive to the caller: %+v", pr.Usage)
	}
	conv, err := st.ConversationUsage(ctx, res.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.TotalTokens != 35514 {
		t.Errorf("conversation total = %d, want 35514", conv.TotalTokens)
	}
}

func TestATurnWithNoMeasurementReportsNoUsage(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done"})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)

	pr, _ := h.Poll(ctx, res.ConversationID, 0)
	if pr.Usage != nil {
		t.Errorf("nothing measured must mean no usage, got %+v", pr.Usage)
	}
}
