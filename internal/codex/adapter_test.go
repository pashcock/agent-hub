package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func newFakeAdapter(t *testing.T) *Adapter {
	t.Helper()
	bin := buildFakeServer(t)
	dir := t.TempDir()
	redial := func(c context.Context) (*Conn, error) { return Dial(c, bin, nil, dir) }

	conn, err := redial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	// The dialer is supplied so the adapter under test can replace a
	// child that dies, exactly as the daemon's does.
	a, err := newAdapter(context.Background(), conn, AdapterOptions{DefaultCwd: dir}, redial)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestStartSessionReturnsThreadID(t *testing.T) {
	a := newFakeAdapter(t)
	sid, err := a.StartSession(context.Background(),
		adapter.StartOptions{Cwd: "/tmp/p", Mode: adapter.ModeCode})
	if err != nil {
		t.Fatal(err)
	}
	if sid != "thread-1" {
		t.Errorf("session id = %q, want thread-1", sid)
	}
}

func TestSendTurnStreamsNormalizedEvents(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p", Mode: adapter.ModeReview})
	ch, err := a.Events(sid)
	if err != nil {
		t.Fatal(err)
	}

	turnID, err := a.SendTurn(ctx, sid, "hi", adapter.TurnOptions{Model: "gpt-5.6-luna", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if turnID != "turn-1" {
		t.Errorf("turn id = %q, want turn-1", turnID)
	}

	var kinds []adapter.EventKind
	var final string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			kinds = append(kinds, ev.Kind)
			if len(ev.Raw) == 0 {
				t.Errorf("event %q lost its raw payload", ev.Kind)
			}
			if ev.Kind == adapter.EventTurnCompleted {
				final = ev.Text
				goto done
			}
		case <-deadline:
			t.Fatalf("timed out; kinds so far = %v", kinds)
		}
	}
done:
	if final != "FINAL" {
		t.Errorf("final text = %q, want FINAL", final)
	}
	want := map[adapter.EventKind]bool{
		adapter.EventTurnStarted:   false,
		adapter.EventMessageDelta:  false,
		adapter.EventTurnCompleted: false,
	}
	for _, k := range kinds {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("event kind %q never arrived; got %v", k, kinds)
		}
	}
}

func TestSandboxDerivedFromMode(t *testing.T) {
	cases := map[adapter.Mode]string{
		adapter.ModeCode:   "workspace-write",
		adapter.ModeReview: "read-only",
		adapter.ModeAsk:    "read-only",
		adapter.ModePlan:   "read-only",
	}
	for mode, want := range cases {
		if got := sandboxFor(adapter.StartOptions{Mode: mode}); got != want {
			t.Errorf("sandboxFor(%q) = %q, want %q", mode, got, want)
		}
	}
	explicit := adapter.StartOptions{Mode: adapter.ModeCode, Sandbox: "read-only"}
	if got := sandboxFor(explicit); got != "read-only" {
		t.Errorf("an explicit sandbox must win, got %q", got)
	}
}

func TestTerminalEventSurvivesAFullBuffer(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"})
	// Do not read the channel. Flood it well past its capacity with
	// droppable deltas, then emit one terminal event.
	for i := 0; i < 5000; i++ {
		a.emit(sid, adapter.Event{Kind: adapter.EventMessageDelta, TurnID: "t", Text: "x"})
	}
	a.emit(sid, adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: "t", Text: "FINAL"})

	ch, _ := a.Events(sid)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == adapter.EventTurnCompleted {
				if ev.Text != "FINAL" {
					t.Fatalf("terminal event text = %q", ev.Text)
				}
				return
			}
		case <-deadline:
			t.Fatal("a terminal event was dropped when the buffer filled")
		}
	}
}

func TestProviderDeathOnIdleSessionReportsColdWithoutInventingAFailure(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"})
	ch, _ := a.Events(sid)

	// Kill the transport underneath the adapter.
	a.killConnForTest()

	// No turn was running, so there is nothing to fail. Failing anyway
	// sends the hub a turn_failed with no turn id, which it resolves
	// against the latest turn of the conversation — an already finished
	// one. The session is gone though, so cold must still arrive.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, open := <-ch:
			if !open {
				t.Fatal("the channel closed without reporting the session cold")
			}
			switch ev.Kind {
			case adapter.EventTurnFailed:
				t.Fatalf("invented a failure for an idle session: %+v", ev)
			case adapter.EventSessionCold:
				return
			}
		case <-deadline:
			t.Fatal("provider death did not report the session cold")
		}
	}
}

func TestCapabilitiesReportsModelsAndMCP(t *testing.T) {
	a := newFakeAdapter(t)
	caps, err := a.Capabilities(context.Background(), "/tmp/p")
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Steer {
		t.Error("Codex must report Steer support")
	}
	if len(caps.Models) != 3 || caps.Models[0].ID != "gpt-5.6-luna" || caps.Models[1].ID != "gpt-6-sol" || caps.Models[2].ID != "gpt-6-luna" {
		t.Fatalf("models = %+v", caps.Models)
	}
	if caps.Models[0].DefaultEffort != "medium" {
		t.Errorf("default effort = %q, want medium", caps.Models[0].DefaultEffort)
	}
	if len(caps.Models[0].Efforts) != 2 {
		t.Errorf("efforts = %v, want two entries", caps.Models[0].Efforts)
	}
	if len(caps.Models[1].Efforts) != 2 || caps.Models[1].Efforts[1] != "ultra" {
		t.Fatalf("second-page efforts = %v", caps.Models[1].Efforts)
	}
	if len(caps.MCPServers) != 1 || caps.MCPServers[0] != "node_repl" {
		t.Errorf("mcp servers = %v", caps.MCPServers)
	}
	if caps.MCPTools["node_repl"] != 1 {
		t.Errorf("MCPTools = %v, want node_repl to expose one tool", caps.MCPTools)
	}
}

func TestCapabilitiesRejectsBrokenModelPagination(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"FAKE_CODEX_MODEL_PAGE_ERROR", "catalog unavailable"},
		{"FAKE_CODEX_MODEL_CURSOR_LOOP", "repeated pagination cursor"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, "1")
			a := newFakeAdapter(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := a.Capabilities(ctx, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}
