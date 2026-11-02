package hub

import (
	"context"
	"github.com/pashcock/agent-hub/internal/store"
	"testing"
)

func TestConsumptionProviderCacheSemantics(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "grok"} {
		u := store.Usage{InputTokens: 100, CachedInput: 80, CacheWrite: 20, TotalTokens: 110}
		if agent == "claude" {
			u.InputTokens = 0
		}
		got := ConsumptionOf(agent, u)
		if got.InputIncludingCache != 100 || got.CacheHitPercent == nil || *got.CacheHitPercent != 80 || got.TotalTokens != 110 {
			t.Fatalf("%s: %+v", agent, got)
		}
	}
	if ConsumptionOf("grok", store.Usage{}).CacheHitPercent != nil {
		t.Fatal("missing input became measured zero hit rate")
	}
}

func TestSnapshotConversationAccountingAcrossTurns(t *testing.T) {
	h, _, s := newHub(t)
	ctx := context.Background()
	id, err := s.CreateConversation(ctx, store.Conversation{PeerAgent: "fake", ProjectCwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{100, 200} {
		turn, err := s.CreateTurn(ctx, store.Turn{ConversationID: id})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RecordUsage(ctx, id, turn, store.Usage{InputTokens: n, CachedInput: n / 2, TotalTokens: n + 10}); err != nil {
			t.Fatal(err)
		}
		if err = s.FinishTurn(ctx, turn, store.TurnCompleted, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, events := range []bool{false, true} {
		p, err := h.poll(ctx, id, 0, events)
		if err != nil {
			t.Fatal(err)
		}
		if p.Usage.TotalTokens != 210 || p.Conversation.Usage.TotalTokens != 320 || p.Conversation.Usage.CachedInputTokens != 150 || p.Conversation.Turns != 2 || p.Conversation.ExecutionMS != nil {
			t.Fatalf("snapshot: %+v", p)
		}
	}
}
