package codex

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func lifecycleCalls(t *testing.T, a *Adapter, params map[string]any) []string {
	t.Helper()
	var calls []string
	if err := a.call(context.Background(), "test/lifecycle", params, &calls); err != nil {
		t.Fatal(err)
	}
	return calls
}

func TestCloseSessionUnsubscribesBeforeForgetting(t *testing.T) {
	for _, status := range []string{"unsubscribed", "notSubscribed", "notLoaded"} {
		t.Run(status, func(t *testing.T) {
			a := newFakeAdapter(t)
			id, err := a.StartSession(context.Background(), adapter.StartOptions{})
			if err != nil {
				t.Fatal(err)
			}
			lifecycleCalls(t, a, map[string]any{"status": status})
			if err := a.CloseSession(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Events(id); err != adapter.ErrSessionGone {
				t.Fatalf("session still registered: %v", err)
			}
			if got := lifecycleCalls(t, a, nil); !reflect.DeepEqual(got, []string{"thread/unsubscribe"}) {
				t.Fatalf("calls: %v", got)
			}
		})
	}
}

func TestCloseSessionFailurePreservesRetry(t *testing.T) {
	for _, params := range []map[string]any{{"fail": true}, {"status": "unexpected"}} {
		a := newFakeAdapter(t)
		id, _ := a.StartSession(context.Background(), adapter.StartOptions{})
		lifecycleCalls(t, a, params)
		if err := a.CloseSession(context.Background(), id); err == nil {
			t.Fatal("close falsely succeeded")
		}
		if _, err := a.Events(id); err != nil {
			t.Fatalf("lost retry state: %v", err)
		}
		lifecycleCalls(t, a, map[string]any{"status": "unsubscribed"})
		if err := a.CloseSession(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloseSessionInterruptsActiveTurn(t *testing.T) {
	a := newFakeAdapter(t)
	id, _ := a.StartSession(context.Background(), adapter.StartOptions{})
	a.threads[id].markTurn("running-turn")
	if err := a.CloseSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleCalls(t, a, nil); !reflect.DeepEqual(got, []string{"turn/interrupt", "thread/unsubscribe"}) {
		t.Fatalf("calls: %v", got)
	}
}

func ageThread(a *Adapter, id string) {
	st := a.threads[id]
	st.mu.Lock()
	st.lastUsed = time.Now().Add(-time.Hour)
	st.mu.Unlock()
}

func TestIdleReleaseAndSendResumeOnSameEventStream(t *testing.T) {
	a := newFakeAdapter(t)
	id, _ := a.StartSession(context.Background(), adapter.StartOptions{Cwd: "/tmp/p", Mode: adapter.ModeReview})
	ch, _ := a.Events(id)
	ageThread(a, id)
	a.evictIdle()
	select {
	case ev := <-ch:
		if ev.Kind != adapter.EventSessionCold {
			t.Fatalf("event %v", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("no cold notification")
	}
	a.evictIdle() // Already cold: do not unsubscribe twice.
	if _, err := a.SendTurn(context.Background(), id, "continue", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleCalls(t, a, nil); !reflect.DeepEqual(got, []string{"thread/unsubscribe", "thread/resume"}) {
		t.Fatalf("calls: %v", got)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == adapter.EventTurnCompleted {
				a.threads[id].mu.Lock()
				turn := a.threads[id].openTurn
				a.threads[id].mu.Unlock()
				if turn != "" {
					t.Fatalf("completed turn resurrected: %q", turn)
				}
				return
			}
		case <-deadline:
			t.Fatal("resumed turn not delivered")
		}
	}
}

func TestIdleReleaseProtectsBusyPendingAndFreshThreads(t *testing.T) {
	for _, mode := range []string{"busy", "approval", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			a := newFakeAdapter(t)
			id, _ := a.StartSession(context.Background(), adapter.StartOptions{})
			if mode != "fresh" {
				ageThread(a, id)
			}
			if mode == "busy" {
				a.threads[id].markTurn("running")
				ageThread(a, id)
			}
			if mode == "approval" {
				a.mu.Lock()
				a.approvals["pending"] = pendingApproval{threadID: id}
				a.mu.Unlock()
			}
			a.evictIdle()
			if got := lifecycleCalls(t, a, nil); len(got) != 0 {
				t.Fatalf("released protected thread: %v", got)
			}
		})
	}
}

func TestIdleReleaseFailureKeepsSubscription(t *testing.T) {
	a := newFakeAdapter(t)
	id, _ := a.StartSession(context.Background(), adapter.StartOptions{})
	ageThread(a, id)
	lifecycleCalls(t, a, map[string]any{"fail": true})
	a.evictIdle()
	a.threads[id].mu.Lock()
	cold := a.threads[id].cold
	a.threads[id].mu.Unlock()
	if cold {
		t.Fatal("failed unsubscribe marked cold")
	}
}
