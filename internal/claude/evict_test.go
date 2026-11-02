package claude

import (
	"context"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

// settle runs one turn so a session is idle and therefore evictable.
func settle(t *testing.T, a *Adapter, sid string, ch <-chan adapter.Event) {
	t.Helper()
	if _, err := a.SendTurn(context.Background(), sid, "hi", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	drainUntil(t, ch, adapter.EventTurnCompleted, 20*time.Second)
}

func waitFor(t *testing.T, what string, cond func() bool, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestCloseRejectsLateProcessAttachment(t *testing.T) {
	a := newAdapter(t, Options{})
	id, ch := startSession(t, a, adapter.ModeAsk)
	settle(t, a, id, ch)
	s, err := a.get(id)
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent resume can finish Spawn after CloseSession removes s.
	late := spawnFake(t, SpawnOptions{})
	if err := a.CloseSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	a.attach(s, late)
	select {
	case <-late.done:
	case <-time.After(time.Second):
		t.Error("late child survived session closure")
	}
	s.mu.Lock()
	hasProc := s.proc != nil
	s.mu.Unlock()
	if hasProc {
		t.Error("closed session accepted a late process")
	}
	if used, _ := a.Capacity(); used != 0 {
		t.Errorf("closed session occupies %d slots", used)
	}
}

func TestCloseCompletedSessionReleasesProcessAndSlot(t *testing.T) {
	a := newAdapter(t, Options{})
	id, ch := startSession(t, a, adapter.ModeAsk)
	settle(t, a, id, ch)
	if used, _ := a.Capacity(); used != 1 {
		t.Fatalf("completed capacity=%d", used)
	}
	s, _ := a.get(id)
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if err := a.CloseSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	default:
		t.Fatal("child not reaped")
	}
	if used, _ := a.Capacity(); used != 0 {
		t.Fatalf("closed capacity=%d", used)
	}
}

func TestOverTheLimitReleasesTheOldestIdleSession(t *testing.T) {
	a := newAdapter(t, Options{MaxLiveProcesses: 2, IdleKill: time.Hour})
	var ids []string
	for i := 0; i < 3; i++ {
		sid, ch := startSession(t, a, adapter.ModeAsk)
		settle(t, a, sid, ch)
		ids = append(ids, sid)
		time.Sleep(30 * time.Millisecond) // keep lastUsed distinct
	}
	waitFor(t, "the process count to come down",
		func() bool { return a.LiveProcesses() <= 2 }, 15*time.Second)
	if a.isLive(ids[0]) {
		t.Error("the oldest idle session must be the one released")
	}
	if !a.isLive(ids[2]) {
		t.Error("the newest session must stay live")
	}
}

func TestReleaseTellsTheHubAndIsNotAFailure(t *testing.T) {
	a := newAdapter(t, Options{MaxLiveProcesses: 1, IdleKill: time.Hour})
	first, ch := startSession(t, a, adapter.ModeAsk)
	settle(t, a, first, ch)

	// A second session pushes the first out.
	startSession(t, a, adapter.ModeAsk)
	waitFor(t, "the first session to be released",
		func() bool { return !a.isLive(first) }, 15*time.Second)

	// The release must be announced, and must not look like a failure.
	deadline := time.After(10 * time.Second)
	sawCold := false
	for !sawCold {
		select {
		case ev := <-ch:
			switch ev.Kind {
			case adapter.EventSessionCold:
				sawCold = true
			case adapter.EventTurnFailed:
				t.Fatalf("releasing an idle session produced a failure: %+v", ev)
			}
		case <-deadline:
			t.Fatal("no session_cold event was emitted")
		}
	}
}

func TestABusySessionIsNeverReleased(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{MaxLiveProcesses: 1, IdleKill: time.Millisecond})
	busy, _ := startSession(t, a, adapter.ModeAsk)
	if _, err := a.SendTurn(ctx, busy, "SLOW work", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	// Give the sweeper many chances to do the wrong thing.
	for i := 0; i < 15; i++ {
		a.evictIfNeeded("")
		time.Sleep(30 * time.Millisecond)
	}
	if !a.isLive(busy) {
		t.Fatal("a session with an outstanding turn must never be released")
	}
}

func TestALongTurnIsNotReleasedTheMomentItFinishes(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{MaxLiveProcesses: 4, IdleKill: 3 * time.Second})
	sid, ch := startSession(t, a, adapter.ModeAsk)

	// The turn takes longer than the idle deadline. lastUsed must be
	// refreshed when it completes, or the session is evictable instantly.
	if _, err := a.SendTurn(ctx, sid, "SLOW one", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	drainUntil(t, ch, adapter.EventTurnCompleted, 60*time.Second)

	a.evictIfNeeded("")
	if !a.isLive(sid) {
		t.Fatal("a session was released immediately after a long turn finished")
	}
}

func TestIdleTimeoutReleasesAndSendBringsItBack(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{MaxLiveProcesses: 4, IdleKill: 200 * time.Millisecond})
	sid, ch := startSession(t, a, adapter.ModeAsk)
	settle(t, a, sid, ch)

	waitFor(t, "the idle session to be released",
		func() bool { return !a.isLive(sid) }, 15*time.Second)

	// The same event channel must keep working across the gap.
	if err := a.ResumeSession(ctx, sid, adapter.StartOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if _, err := a.SendTurn(ctx, sid, "still here", adapter.TurnOptions{}); err != nil {
		t.Fatalf("SendTurn after release: %v", err)
	}
	ev := drainUntil(t, ch, adapter.EventTurnCompleted, 30*time.Second)
	if ev.Text != "ack: still here" {
		t.Errorf("result after resume = %q", ev.Text)
	}
}
