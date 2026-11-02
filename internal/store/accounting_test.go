package store

import (
	"context"
	"testing"
	"time"
)

func TestConversationCoverageMissingAndMeasured(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	id := seedConversation(t, s)
	c, err := s.ConversationCoverage(ctx, id)
	if err != nil || c.ExecutionMS != nil || c.Turns != 0 {
		t.Fatalf("empty: %+v %v", c, err)
	}
	old, err := s.CreateTurn(ctx, Turn{ConversationID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTurn(ctx, old, TurnCompleted, ""); err != nil {
		t.Fatal(err)
	}
	measured, err := s.CreateTurn(ctx, Turn{ConversationID: id})
	if err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(1000)
	if err = s.BeginTiming(ctx, measured, at, at, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE turns SET completed_at=2500,status=? WHERE id=?`, TurnCompleted, measured); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.RecordUsage(ctx, id, measured, Usage{TotalTokens: 100, CachedInput: 70}); err != nil {
			t.Fatal(err)
		}
	}
	active, err := s.CreateTurn(ctx, Turn{ConversationID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginTiming(ctx, active, at, at, at, at); err != nil {
		t.Fatal(err)
	}
	c, err = s.ConversationCoverage(ctx, id)
	if err != nil || c.Turns != 3 || c.TimedTurns != 1 || c.UsageReportedTurns != 1 || c.ExecutionMS == nil || *c.ExecutionMS != 1500 {
		t.Fatalf("coverage: %+v %v", c, err)
	}
	u, err := s.ConversationUsage(ctx, id)
	if err != nil || u.TotalTokens != 100 || u.CachedInput != 70 {
		t.Fatalf("usage: %+v %v", u, err)
	}
}
