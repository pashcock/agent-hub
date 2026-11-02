package claude

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func newAdapter(t *testing.T, o Options) *Adapter {
	t.Helper()
	if o.Bin == "" {
		o.Bin = buildFakeClaude(t)
	}
	a := NewAdapter(o)
	t.Cleanup(func() { a.Close() })
	return a
}

func drainUntil(t *testing.T, ch <-chan adapter.Event, kind adapter.EventKind, within time.Duration) adapter.Event {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, open := <-ch:
			if !open {
				t.Fatalf("stream closed before %q arrived", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", kind)
		}
	}
}

func startSession(t *testing.T, a *Adapter, mode adapter.Mode) (string, <-chan adapter.Event) {
	t.Helper()
	sid, err := a.StartSession(context.Background(),
		adapter.StartOptions{Cwd: t.TempDir(), Mode: mode})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	ch, err := a.Events(sid)
	if err != nil {
		t.Fatal(err)
	}
	return sid, ch
}

func TestStartSessionReturnsBeforeTheProviderSpeaks(t *testing.T) {
	a := newAdapter(t, Options{})
	start := time.Now()
	sid, _ := startSession(t, a, adapter.ModeReview)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("StartSession took %v; it must not wait for system/init", elapsed)
	}
	if len(sid) != 36 {
		t.Errorf("session id %q is not a UUID; the hub must name the session", sid)
	}
}

func TestOneTurnCompletes(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{})
	sid, ch := startSession(t, a, adapter.ModeReview)
	turnID, err := a.SendTurn(ctx, sid, "hello", adapter.TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ev := drainUntil(t, ch, adapter.EventTurnCompleted, 20*time.Second)
	if ev.TurnID != turnID {
		t.Errorf("completion carried turn %q, want %q", ev.TurnID, turnID)
	}
	if ev.Text != "ack: hello" {
		t.Errorf("final text = %q", ev.Text)
	}
}

func TestTurnIDsMatchResultsInOrder(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{})
	sid, ch := startSession(t, a, adapter.ModeAsk)

	// Send both before either finishes, so the match is really a FIFO
	// and not just two independent round trips.
	first, _ := a.SendTurn(ctx, sid, "SLOW one", adapter.TurnOptions{})
	second, _ := a.SendTurn(ctx, sid, "two", adapter.TurnOptions{})

	e1 := drainUntil(t, ch, adapter.EventTurnCompleted, 60*time.Second)
	e2 := drainUntil(t, ch, adapter.EventTurnCompleted, 60*time.Second)
	if e1.TurnID != first || e2.TurnID != second {
		t.Fatalf("turn ids mismatched: got %q,%q want %q,%q",
			e1.TurnID, e2.TurnID, first, second)
	}
}

func TestInterruptClosesTheTurnExactlyOnce(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{})
	sid, ch := startSession(t, a, adapter.ModeAsk)
	turnID, _ := a.SendTurn(ctx, sid, "SLOW work", adapter.TurnOptions{})
	time.Sleep(500 * time.Millisecond)

	if err := a.Interrupt(ctx, sid, turnID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	ev := drainUntil(t, ch, adapter.EventInterrupted, 30*time.Second)
	if ev.TurnID != turnID {
		t.Errorf("interrupted turn = %q, want %q", ev.TurnID, turnID)
	}

	// The provider's late result must not produce a second terminal
	// event, and must not shift the id of the next turn.
	next, _ := a.SendTurn(ctx, sid, "after", adapter.TurnOptions{})
	done := drainUntil(t, ch, adapter.EventTurnCompleted, 30*time.Second)
	if done.TurnID != next {
		t.Fatalf("the FIFO desynchronised: completion carried %q, want %q",
			done.TurnID, next)
	}
}

func TestSteerIsUnsupportedAndDeliversNothing(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{})
	sid, ch := startSession(t, a, adapter.ModeAsk)
	turnID, _ := a.SendTurn(ctx, sid, "SLOW one", adapter.TurnOptions{})

	if err := a.Steer(ctx, sid, turnID, "extra context"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("Steer err = %v, want ErrUnsupported", err)
	}
	// Exactly one turn must come back. A Steer that quietly delivered the
	// message would produce a second result the hub has no row for.
	drainUntil(t, ch, adapter.EventTurnCompleted, 60*time.Second)
	select {
	case ev := <-ch:
		if ev.Kind == adapter.EventTurnCompleted {
			t.Fatalf("Steer delivered a hidden turn: %+v", ev)
		}
	case <-time.After(3 * time.Second):
	}
}

func TestApproveIsUnsupported(t *testing.T) {
	a := newAdapter(t, Options{})
	sid, _ := startSession(t, a, adapter.ModeAsk)
	if err := a.Approve(context.Background(), sid, "any", adapter.Decision{Verb: "accept"}); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("Approve err = %v, want ErrUnsupported", err)
	}
}

func TestProfileIsStaticAndHonest(t *testing.T) {
	p := newAdapter(t, Options{}).Profile()
	if p.Steer {
		t.Error("Claude must not claim mid-turn steering")
	}
	if p.SandboxedReadOnly {
		t.Error("Claude has no filesystem sandbox; flags enforce read-only")
	}
	if p.Approvals {
		t.Error("Claude raises no permission requests in -p mode")
	}
}

func TestProcessDeathFailsTheOpenTurn(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{})
	sid, ch := startSession(t, a, adapter.ModeAsk)
	turnID, _ := a.SendTurn(ctx, sid, "SLOW three", adapter.TurnOptions{})
	time.Sleep(500 * time.Millisecond)

	a.killForTest(sid)

	ev := drainUntil(t, ch, adapter.EventTurnFailed, 30*time.Second)
	if ev.TurnID != turnID {
		t.Errorf("failed turn = %q, want %q", ev.TurnID, turnID)
	}
	if ev.Err == "" {
		t.Error("a process-death failure must carry a reason")
	}
}

func TestTerminalEventSurvivesAnUnreadChannel(t *testing.T) {
	ctx := context.Background()
	a := newAdapter(t, Options{})
	sid, ch := startSession(t, a, adapter.ModeAsk)

	// Nobody reads ch while these run. A terminal event that blocked here
	// would stop the reader draining stdout and hang the provider.
	var ids []string
	for i := 0; i < 40; i++ {
		id, err := a.SendTurn(ctx, sid, "chatter", adapter.TurnOptions{})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	seen := map[string]bool{}
	deadline := time.After(90 * time.Second)
	for len(seen) < len(ids) {
		select {
		case ev := <-ch:
			if ev.Kind == adapter.EventTurnCompleted {
				seen[ev.TurnID] = true
			}
		case <-deadline:
			t.Fatalf("only %d of %d completions arrived", len(seen), len(ids))
		}
	}
}

func TestReadOnlyModesSetTheRightFlags(t *testing.T) {
	cases := map[adapter.Mode]struct {
		mode   string
		denies bool
	}{
		// Never plan mode: it refuses every MCP call, which strips a
		// peer of the project's own research tooling.
		adapter.ModeCode:   {"auto", false},
		adapter.ModeReview: {"auto", true},
		adapter.ModeAsk:    {"auto", true},
		adapter.ModePlan:   {"auto", true},
	}
	for mode, want := range cases {
		got := spawnOptionsFor(Options{MaskMCPServer: "agent-hub"},
			adapter.StartOptions{Cwd: "/tmp/p", Mode: mode})
		if got.PermissionMode != want.mode {
			t.Errorf("mode %q -> permission mode %q, want %q", mode, got.PermissionMode, want.mode)
		}
		denies := false
		for _, d := range got.DisallowedTools {
			if d == "Write" {
				denies = true
			}
		}
		if denies != want.denies {
			t.Errorf("mode %q -> disallowed %v, want write denial = %v",
				mode, got.DisallowedTools, want.denies)
		}
		if got.MaskMCPServer != "agent-hub" {
			t.Errorf("mode %q lost the hub mask", mode)
		}
	}
}

func TestEnvironmentIsInheritedMinusTheToken(t *testing.T) {
	t.Setenv("AGENT_HUB_TOKEN", "secret")
	t.Setenv("PROJECT_THING", "keep-me")
	env := filterEnv(os.Environ(), "AGENT_HUB_TOKEN")
	var sawToken, sawThing bool
	for _, kv := range env {
		if kv == "AGENT_HUB_TOKEN=secret" {
			sawToken = true
		}
		if kv == "PROJECT_THING=keep-me" {
			sawThing = true
		}
	}
	if sawToken {
		t.Error("the hub token must not reach a peer")
	}
	if !sawThing {
		t.Error("the rest of the environment must be inherited; project MCP servers need it")
	}
}
