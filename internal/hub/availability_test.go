package hub

import (
	"context"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/store"
)

func TestPauseGatesAdmissionsNotActiveTurns(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	r, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetProviderPause(ctx, "fake", "quota", time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Start(ctx, StartRequest{Agent: "fake", Task: "two", Cwd: "/tmp/p", Queue: true}); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("pause bypassed: %v", err)
	}
	c, _ := st.GetConversation(ctx, r.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: r.TurnID, Text: "done"})
	waitStatus(t, h, r.ConversationID, store.TurnCompleted)
	if _, err = h.Send(ctx, r.ConversationID, "next", adapter.TurnOptions{}); err == nil {
		t.Fatal("send bypassed pause")
	}
	if _, err = h.Capabilities(ctx, "fake", "/tmp/p"); err == nil {
		t.Fatal("probe bypassed pause")
	}
	if !h.AgentStatuses()[0].Availability.Paused {
		t.Fatal("pause missing from discovery")
	}
	if err = h.SetProviderPause(ctx, "fake", "", time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Send(ctx, r.ConversationID, "next", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
}
func TestPauseKeepsQueueUntilResume(t *testing.T) {
	ctx := context.Background()
	h, _, st := newCapped(t, 1)
	first, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p"})
	queued, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "two", Cwd: "/tmp/p", Queue: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.SetProviderPause(ctx, "fake", "quota", time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	if err = h.CloseConversation(ctx, first.ConversationID); err != nil {
		t.Fatal(err)
	}
	h.promoteQueued(ctx)
	c, _ := st.GetConversation(ctx, queued.ConversationID)
	if c.Status != store.StatusQueued || c.NativeSessionID != "" {
		t.Fatalf("queue escaped: %+v", c)
	}
	if err = h.SetProviderPause(ctx, "fake", "", time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, h, queued.ConversationID, store.TurnRunning)
}
func TestTimingCapturesActivityAndText(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	r, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetConversation(ctx, r.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventActivity, TurnID: r.TurnID, Activity: "working"})
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventMessageDelta, TurnID: r.TurnID, Text: "answer"})
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: r.TurnID, Text: "answer"})
	p := waitStatus(t, h, r.ConversationID, store.TurnCompleted)
	if p.Timings == nil || p.Timings.FirstTextMS == nil || p.Timings.FirstActivityMS == nil {
		t.Fatalf("timing missing: %+v", p.Timings)
	}
}

func TestSnapshotCursorIsTailButPollRetainsIncrementalCursor(t *testing.T) {
	ctx := context.Background()
	h, _, st := newHub(t)
	id, err := st.CreateConversation(ctx, store.Conversation{PeerAgent: "fake", ProjectCwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 650; i++ {
		if err := st.AppendEvent(ctx, id, "", "raw", nil); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := h.Snapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := h.Poll(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor != 650 || len(snapshot.Events) != 0 || poll.Cursor != 200 || len(poll.Events) != 200 {
		t.Fatalf("cursor contracts: snapshot=%d poll=%d", snapshot.Cursor, poll.Cursor)
	}
}

type resumeRecorder struct {
	adapter.Adapter
	got adapter.StartOptions
}

func (a *resumeRecorder) ResumeSession(ctx context.Context, id string, opts adapter.StartOptions) error {
	a.got = opts
	return a.Adapter.ResumeSession(ctx, id, opts)
}
func TestHubColdResumePreservesSandboxAndEffort(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fa := fake.New()
	record := &resumeRecorder{Adapter: fa}
	h := New(st, map[string]adapter.Adapter{"fake": record}, Config{})
	defer h.Close()
	r, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "one", Cwd: "/tmp/p", Mode: "code", Sandbox: "read-only", Model: "test", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetConversation(ctx, r.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: r.TurnID, Text: "done"})
	waitStatus(t, h, r.ConversationID, store.TurnCompleted)
	if err = st.SetConversationStatus(ctx, r.ConversationID, store.StatusCold); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Send(ctx, r.ConversationID, "next", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	if record.got.Sandbox != "read-only" || record.got.Effort != "high" || record.got.Model != "test" {
		t.Fatalf("lost launch options: %+v", record.got)
	}
}
