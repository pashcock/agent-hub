package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPausePersistenceAndExpiry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetProviderPause(ctx, "grok", "quota", time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.ProviderPause(ctx, "grok")
	if err != nil || !p.Paused || p.Reason != "quota" {
		t.Fatalf("lost pause: %+v %v", p, err)
	}
	if err = s.SetProviderPause(ctx, "grok", "expired", time.Now().Add(-time.Second), true); err != nil {
		t.Fatal(err)
	}
	p, err = s.ProviderPause(ctx, "grok")
	if err != nil || p.Paused {
		t.Fatalf("expired pause: %+v %v", p, err)
	}
}
func TestLatestEventsUsesTailAndCursor(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	id := seedConversation(t, s)
	for i := 0; i < 650; i++ {
		if err := s.AppendEvent(ctx, id, "", "raw", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.LatestEvents(ctx, id, 0, 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 120 || events[0].Seq != 650 || events[119].Seq != 531 {
		t.Fatalf("wrong tail: %d %+v", len(events), events[0])
	}
	older, err := s.LatestEvents(ctx, id, events[119].Seq, 120)
	if err != nil || older[0].Seq != 530 {
		t.Fatalf("wrong cursor: %v %v", older, err)
	}
}
func TestTimingMissingSamplesAndApprovalUnion(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	id := seedConversation(t, s)
	s.CreateTurn(ctx, Turn{ID: "t", ConversationID: id})
	if m, err := s.TurnTimings(ctx, "t"); err != nil || m != nil {
		t.Fatalf("fabricated history: %+v %v", m, err)
	}
	base := time.Now().Add(-10 * time.Second)
	s.BeginTiming(ctx, "t", base, base.Add(time.Second), base.Add(2*time.Second), base.Add(3*time.Second))
	s.ObserveTiming(ctx, "t", "approval_requested", base.Add(4*time.Second))
	s.ObserveTiming(ctx, "t", "approval_requested", base.Add(5*time.Second))
	s.EndApprovalTiming(ctx, "t", base.Add(6*time.Second))
	s.EndApprovalTiming(ctx, "t", base.Add(7*time.Second))
	m, err := s.TurnTimings(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if m.QueueMS != 1000 || m.SetupMS != 1000 || m.SubmitMS != 1000 || m.ApprovalMS != 2000 || m.FirstTextMS != nil {
		t.Fatalf("bad timing: %+v", m)
	}
}
func TestSearchExactWorkspaceAndLiteralQuery(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for _, cwd := range []string{"/tmp/proj", "/tmp/proj-worktree"} {
		id, err := s.CreateConversation(ctx, Conversation{PeerAgent: "grok", ProjectCwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		s.AppendMessage(ctx, id, "", "user", "Fix 100% of bug_42")
	}
	rows, n, err := s.SearchConversations(ctx, ConversationFilter{Cwd: "/tmp/proj", Query: "100%"})
	if err != nil || n != 1 || len(rows) != 1 {
		t.Fatalf("search: %v %d %v", rows, n, err)
	}
	_, n, err = s.SearchConversations(ctx, ConversationFilter{Query: "%' OR 1=1 --"})
	if err != nil || n != 0 {
		t.Fatalf("query not literal: %d %v", n, err)
	}
}

func TestModelMetricsExcludeOtherModelsInSameConversation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	id := seedConversation(t, s)
	for _, model := range []string{"model-a", "model-b"} {
		tid, err := s.CreateTurn(ctx, Turn{ID: model, ConversationID: id, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RecordUsage(ctx, id, tid, Usage{Model: model, TotalTokens: 10}); err != nil {
			t.Fatal(err)
		}
		if err = s.FinishTurn(ctx, tid, TurnCompleted, ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ModelMetrics(ctx, ConversationFilter{Model: "model-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Model != "model-b" || rows[0].Turns != 1 || rows[0].Measured != 0 || rows[0].AverageTotalMS != nil {
		t.Fatalf("mixed or fabricated metric samples: %+v", rows)
	}
}

func TestSearchKeepsOlderActiveWorkOnFirstPage(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	active := seedConversation(t, s)
	s.CreateTurn(ctx, Turn{ID: "active", ConversationID: active})
	for i := 0; i < 55; i++ {
		seedConversation(t, s)
	}
	rows, count, err := s.SearchConversations(ctx, ConversationFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if count != 56 || len(rows) != 50 || rows[0].ID != active {
		t.Fatal("active conversation buried behind finished history")
	}
}

func TestLegacyDatabaseMigratesAndPersistsSandbox(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(schemaSQL, "    sandbox           TEXT NOT NULL DEFAULT '',\n", "", 1)
	if _, err = db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateConversation(ctx, Conversation{PeerAgent: "codex", ProjectCwd: "/tmp/p", Mode: "code", Sandbox: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.GetConversation(ctx, id)
	if err != nil || c.Sandbox != "read-only" {
		t.Fatalf("sandbox lost: %+v %v", c, err)
	}
}
func TestMetricsExcludeFastFailuresAndZeroApprovalSamples(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	id := seedConversation(t, s)
	for i, status := range []string{TurnCompleted, TurnCompleted, TurnFailed, TurnInterrupted} {
		tid := fmt.Sprintf("timed-%d", i)
		s.CreateTurn(ctx, Turn{ID: tid, ConversationID: id, Model: "test"})
		start := time.Now().Add(-10 * time.Second)
		if i >= 2 {
			start = time.Now()
		}
		s.BeginTiming(ctx, tid, start, start, start, start)
		if i == 0 {
			s.ObserveTiming(ctx, tid, "approval_requested", start.Add(time.Second))
			s.EndApprovalTiming(ctx, tid, start.Add(3*time.Second))
		}
		s.FinishTurn(ctx, tid, status, "")
	}
	rows, err := s.ModelMetrics(ctx, ConversationFilter{})
	if err != nil {
		t.Fatal(err)
	}
	m := rows[0]
	if m.Measured != 2 || m.ApprovalSamples != 1 || m.AverageTotalMS == nil || *m.AverageTotalMS < 9900 || m.AverageApprovalMS == nil || *m.AverageApprovalMS != 2000 {
		t.Fatalf("bad comparison: %+v", m)
	}
}
func TestHistoryPagesStayBoundedAndDoNotSkipMessages(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	id := seedConversation(t, s)
	for i := 0; i < 250; i++ {
		tid := fmt.Sprintf("page-%d", i)
		s.CreateTurn(ctx, Turn{ID: tid, ConversationID: id})
		s.AppendMessage(ctx, id, tid, "user", tid)
	}
	var before int64
	seen := map[string]bool{}
	for {
		messages, next, err := s.MessagePage(ctx, id, before)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) > 100 {
			t.Fatal("unbounded messages")
		}
		for _, m := range messages {
			if seen[m.ID] {
				t.Fatal("duplicate message")
			}
			seen[m.ID] = true
		}
		if next == 0 {
			break
		}
		before = next
	}
	if len(seen) != 250 {
		t.Fatalf("lost messages: %d", len(seen))
	}
	turns, next, err := s.TurnPage(ctx, id, 0)
	if err != nil || len(turns) != 50 || next == 0 {
		t.Fatalf("turn page: %d %d %v", len(turns), next, err)
	}
}
