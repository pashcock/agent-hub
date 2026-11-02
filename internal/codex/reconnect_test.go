package codex

import (
	"context"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func TestAdapterRecoversWhenTheAppServerDies(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	if _, err := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"}); err != nil {
		t.Fatal(err)
	}

	// The child dies the way it would if it crashed or was killed.
	a.killConnForTest()

	// A new session must become possible again without restarting the
	// hub. Before this the adapter stayed dead for the daemon's life.
	deadline := time.After(30 * time.Second)
	for {
		if _, err := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"}); err == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the adapter never recovered after its app-server died")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestDeathFailsOpenTurnsAndReportsTheSessionCold(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"})
	ch, _ := a.Events(sid)
	turnID, err := a.SendTurn(ctx, sid, "hi", adapter.TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}

	a.killConnForTest()

	// Both signals matter: the turn must fail, and the hub must be told
	// the conversation is no longer live, or peer_send would skip
	// rehydration and talk to a session that is gone.
	var sawFail, sawCold bool
	deadline := time.After(30 * time.Second)
	for !(sawFail && sawCold) {
		select {
		case ev, open := <-ch:
			if !open {
				t.Fatalf("stream closed with fail=%v cold=%v", sawFail, sawCold)
			}
			switch ev.Kind {
			case adapter.EventTurnFailed:
				if ev.TurnID != turnID {
					t.Errorf("failed turn = %q, want %q", ev.TurnID, turnID)
				}
				if ev.Err == "" {
					t.Error("a death failure must carry a reason")
				}
				sawFail = true
			case adapter.EventSessionCold:
				sawCold = true
			}
		case <-deadline:
			t.Fatalf("timed out with fail=%v cold=%v", sawFail, sawCold)
		}
	}
}

func TestCloseStopsReconnecting(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"})
	a.Close()

	// A deliberate shutdown must not be mistaken for a crash and
	// respawn a child that nobody will ever reap.
	time.Sleep(500 * time.Millisecond)
	if _, err := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"}); err == nil {
		t.Fatal("a closed adapter must stay closed")
	}
}
