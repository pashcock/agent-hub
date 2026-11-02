//go:build integration

package codex

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

// realAdapter starts the actual codex app-server. The test skips when the
// binary is absent so the suite still runs on a machine without Codex.
func realAdapter(t *testing.T) *Adapter {
	t.Helper()
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed; skipping the real integration test")
	}
	ctx := context.Background()
	conn, err := Dial(ctx, bin, append([]string{"app-server"}, MaskArgs("agent-hub")...), t.TempDir())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	a, err := NewAdapter(ctx, conn, AdapterOptions{DefaultCwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func awaitCompletion(t *testing.T, ch <-chan adapter.Event, within time.Duration) adapter.Event {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev := <-ch:
			switch ev.Kind {
			case adapter.EventTurnCompleted, adapter.EventTurnFailed, adapter.EventInterrupted:
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to finish")
		}
	}
}

func TestRealCodexRoundTripAndResume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the real provider test in short mode")
	}
	ctx := context.Background()
	dir := t.TempDir()

	a := realAdapter(t)
	sid, err := a.StartSession(ctx, adapter.StartOptions{
		Cwd: dir, Mode: adapter.ModeAsk, Effort: "low"})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	ch, _ := a.Events(sid)

	if _, err := a.SendTurn(ctx, sid,
		"My codeword is PLAN-4711. Reply with exactly OK and nothing else.",
		adapter.TurnOptions{}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	awaitCompletion(t, ch, 3*time.Minute)

	// A second turn in the same thread must remember the codeword.
	if _, err := a.SendTurn(ctx, sid,
		"Repeat my codeword exactly, nothing else.", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	ev := awaitCompletion(t, ch, 3*time.Minute)
	if !strings.Contains(ev.Text, "PLAN-4711") {
		t.Fatalf("second turn lost the context: %q", ev.Text)
	}

	// Kill the whole app-server and resume the thread from disk.
	a.Close()
	b := realAdapter(t)
	if err := b.ResumeSession(ctx, sid, adapter.StartOptions{Cwd: dir, Mode: adapter.ModeAsk}); err != nil {
		t.Fatalf("ResumeSession after process death: %v", err)
	}
	ch2, _ := b.Events(sid)
	if _, err := b.SendTurn(ctx, sid,
		"Repeat my codeword exactly, nothing else.", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	ev2 := awaitCompletion(t, ch2, 3*time.Minute)
	if !strings.Contains(ev2.Text, "PLAN-4711") {
		t.Fatalf("thread/resume lost the context: %q", ev2.Text)
	}
}

// TestRealCodexCannotSeeAgentHub proves both halves of the peer-masking
// guarantee, separately, against the real binary and a running daemon.
//
// Three processes are compared:
//
//	reference  full environment, no mask   -> must see the hub's tools
//	token only full environment is denied  -> the token strip alone suffices
//	production the mask and the strip      -> sees nothing
//
// The reference case is what stops this passing vacuously: without it, a
// hub that was simply unreachable would look like a successful mask.
func TestRealCodexCannotSeeAgentHub(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the real provider test in short mode")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	if os.Getenv("AGENT_HUB_TOKEN") == "" {
		t.Skip("AGENT_HUB_TOKEN is unset, so no process can reach the hub; " +
			"export it from ~/.local/share/agent-hub/token")
	}
	ctx := context.Background()

	// tools reports how many tools agent-hub exposes to one process. MCP
	// servers hand over their tool lists a few seconds after startup, so
	// it polls while it is still expecting a non-zero answer.
	tools := func(label string, masked, keepToken bool, expectTools bool) int {
		t.Helper()
		args := []string{"app-server"}
		if masked {
			args = append(args, MaskArgs("agent-hub")...)
		}
		env := filterEnv(os.Environ(), "AGENT_HUB_TOKEN")
		if keepToken {
			env = os.Environ()
		}
		conn, err := dial(ctx, bin, args, t.TempDir(), env)
		if err != nil {
			t.Fatalf("dial(%s): %v", label, err)
		}
		a, err := NewAdapter(ctx, conn, AdapterOptions{DefaultCwd: t.TempDir()})
		if err != nil {
			t.Fatalf("NewAdapter(%s): %v", label, err)
		}
		defer a.Close()

		var caps adapter.Capabilities
		deadline := time.Now().Add(45 * time.Second)
		for {
			caps, err = a.Capabilities(ctx, t.TempDir())
			if err != nil {
				t.Fatalf("Capabilities(%s): %v", label, err)
			}
			if !expectTools || caps.MCPTools["agent-hub"] > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		if len(caps.Models) == 0 {
			t.Fatalf("model/list returned no models (%s)", label)
		}
		t.Logf("%s: agent-hub tools=%d, servers=%v", label,
			caps.MCPTools["agent-hub"], caps.MCPServers)
		return caps.MCPTools["agent-hub"]
	}

	reference := tools("reference: no mask, token present", false, true, true)
	if reference == 0 {
		t.Skip("agent-hub exposes no tools even to a process that can reach it. " +
			"Register it with `codex mcp add agent-hub --url ...` and start the " +
			"daemon; until then this test cannot prove anything.")
	}

	if n := tools("token strip only", false, false, false); n != 0 {
		t.Errorf("stripping the token alone left %d agent-hub tools visible", n)
	}
	if n := tools("production: mask and strip", true, false, false); n != 0 {
		t.Errorf("a masked peer still sees %d agent-hub tools", n)
	}
}
