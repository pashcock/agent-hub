package store

import (
	"context"
	"encoding/json"
	"testing"
)

func seedConversation(t *testing.T, s *Store) string {
	t.Helper()
	id, err := s.CreateConversation(context.Background(),
		Conversation{PeerAgent: "codex", ProjectCwd: "/tmp/proj"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTurnLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	conv := seedConversation(t, s)

	turnID, err := s.CreateTurn(ctx, Turn{
		ID: "turn-1", ConversationID: conv, Model: "gpt-6-astra", Effort: "high",
	})
	if err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	if turnID != "turn-1" {
		t.Errorf("CreateTurn returned %q, want the supplied id turn-1", turnID)
	}

	got, err := s.GetTurn(ctx, "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TurnRunning {
		t.Errorf("new turn Status = %q, want %q", got.Status, TurnRunning)
	}

	if err := s.FinishTurn(ctx, "turn-1", TurnCompleted, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTurn(ctx, "turn-1")
	if got.Status != TurnCompleted {
		t.Errorf("Status = %q, want %q", got.Status, TurnCompleted)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt must be set once a turn finishes")
	}
}

func TestFinishTurnRejectsNonTerminalState(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	conv := seedConversation(t, s)
	s.CreateTurn(ctx, Turn{ID: "t", ConversationID: conv})
	if err := s.FinishTurn(ctx, "t", TurnRunning, ""); err == nil {
		t.Fatal("FinishTurn must reject a non-terminal state")
	}
}

func TestLatestTurn(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	conv := seedConversation(t, s)
	s.CreateTurn(ctx, Turn{ID: "old", ConversationID: conv})
	s.FinishTurn(ctx, "old", TurnCompleted, "")
	s.CreateTurn(ctx, Turn{ID: "new", ConversationID: conv})

	got, err := s.LatestTurn(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "new" {
		t.Errorf("LatestTurn = %q, want new", got.ID)
	}
}

func TestLatestTurnWithNoTurns(t *testing.T) {
	s := openTemp(t)
	conv := seedConversation(t, s)
	if _, err := s.LatestTurn(context.Background(), conv); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMessagesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	conv := seedConversation(t, s)
	s.CreateTurn(ctx, Turn{ID: "t1", ConversationID: conv})

	if err := s.AppendMessage(ctx, conv, "t1", "user", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(ctx, conv, "t1", "assistant", "hi"); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "hello" {
		t.Errorf("first message = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("messages must come back in insertion order, got %+v", msgs)
	}
}

func TestEventsSinceIsAMonotonicCursor(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	conv := seedConversation(t, s)

	for i := 0; i < 3; i++ {
		if err := s.AppendEvent(ctx, conv, "t1", "message_delta",
			json.RawMessage(`{"i":`+string(rune('0'+i))+`}`)); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.EventsSince(ctx, conv, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}
	if all[0].Seq >= all[1].Seq || all[1].Seq >= all[2].Seq {
		t.Fatalf("Seq must be strictly increasing: %d %d %d",
			all[0].Seq, all[1].Seq, all[2].Seq)
	}

	rest, err := s.EventsSince(ctx, conv, all[0].Seq, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 {
		t.Fatalf("EventsSince must be exclusive of the cursor, got %d want 2", len(rest))
	}

	limited, err := s.EventsSince(ctx, conv, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("limit ignored: got %d want 2", len(limited))
	}
}
