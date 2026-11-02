package fake

import (
	"context"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func TestStartSendAndEvents(t *testing.T) {
	ctx := context.Background()
	a := New()
	defer a.Close()

	sid, err := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp", Mode: adapter.ModeCode})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Events(sid)
	if err != nil {
		t.Fatal(err)
	}

	turnID, err := a.SendTurn(ctx, sid, "hello", adapter.TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if turnID == "" {
		t.Fatal("SendTurn must return a turn id")
	}

	a.Emit(sid, adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: turnID, Text: "done"})

	select {
	case ev := <-ch:
		if ev.Kind != adapter.EventTurnStarted {
			t.Fatalf("first event = %q, want %q", ev.Kind, adapter.EventTurnStarted)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for turn_started")
	}
	select {
	case ev := <-ch:
		if ev.Kind != adapter.EventTurnCompleted || ev.Text != "done" {
			t.Fatalf("second event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for turn_completed")
	}
}

func TestSendTurnOnUnknownSession(t *testing.T) {
	a := New()
	defer a.Close()
	if _, err := a.SendTurn(context.Background(), "nope", "x", adapter.TurnOptions{}); err != adapter.ErrSessionGone {
		t.Fatalf("err = %v, want ErrSessionGone", err)
	}
}

func TestResumeFailureIsProgrammable(t *testing.T) {
	ctx := context.Background()
	a := New()
	defer a.Close()
	a.FailNextResume()
	if err := a.ResumeSession(ctx, "anything", adapter.StartOptions{Cwd: "/tmp"}); err != adapter.ErrSessionGone {
		t.Fatalf("err = %v, want ErrSessionGone", err)
	}
	// The failure is one-shot.
	if err := a.ResumeSession(ctx, "anything", adapter.StartOptions{Cwd: "/tmp"}); err != nil {
		t.Fatalf("second resume must succeed, got %v", err)
	}
}

func TestSteerIsSupportedAndInterruptEmits(t *testing.T) {
	ctx := context.Background()
	a := New()
	defer a.Close()
	sid, _ := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp"})
	ch, _ := a.Events(sid)
	turnID, _ := a.SendTurn(ctx, sid, "long task", adapter.TurnOptions{})
	<-ch // turn_started

	if err := a.Steer(ctx, sid, turnID, "extra context"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if err := a.Interrupt(ctx, sid, turnID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kind != adapter.EventInterrupted {
			t.Fatalf("event = %q, want %q", ev.Kind, adapter.EventInterrupted)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for turn_interrupted")
	}
}
