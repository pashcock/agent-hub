//go:build integration

package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func realAdapter(t *testing.T, o Options) *Adapter {
	t.Helper()
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed; skipping the real integration test")
	}
	o.Bin = bin
	if o.MaskMCPServer == "" {
		o.MaskMCPServer = "agent-hub"
	}
	a := NewAdapter(o)
	t.Cleanup(func() { a.Close() })
	return a
}

func finalText(t *testing.T, ch <-chan adapter.Event, within time.Duration) string {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, open := <-ch:
			if !open {
				t.Fatal("the stream closed before the turn finished")
			}
			switch ev.Kind {
			case adapter.EventTurnCompleted:
				return ev.Text
			case adapter.EventTurnFailed:
				t.Fatalf("turn failed: %s", ev.Err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to finish")
		}
	}
}

func TestRealClaudeSurvivesProcessDeath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the real provider test in short mode")
	}
	ctx := context.Background()
	work := t.TempDir()
	a := realAdapter(t, Options{MaxLiveProcesses: 2, IdleKill: time.Hour})

	sid, err := a.StartSession(ctx, adapter.StartOptions{
		Cwd: work, Mode: adapter.ModeAsk, Model: "haiku"})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	ch, _ := a.Events(sid)
	if _, err := a.SendTurn(ctx, sid,
		"My codeword is CACAO-3312. Reply with exactly OK and nothing else.",
		adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	finalText(t, ch, 3*time.Minute)

	a.killForTest(sid)
	deadline := time.After(30 * time.Second)
	for a.isLive(sid) {
		select {
		case <-deadline:
			t.Fatal("the process never went away")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := a.ResumeSession(ctx, sid, adapter.StartOptions{
		Cwd: work, Mode: adapter.ModeAsk, Model: "haiku"}); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if _, err := a.SendTurn(ctx, sid,
		"Repeat my codeword exactly, nothing else.", adapter.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := finalText(t, ch, 3*time.Minute); !strings.Contains(got, "CACAO-3312") {
		t.Fatalf("resume lost the context: %q", got)
	}
}

func TestRealClaudeReviewModeCannotWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the real provider test in short mode")
	}
	ctx := context.Background()
	ask := "Please create a file called notes.txt in this project directory " +
		"with the single line: hello. Use the Write tool. " +
		"If a permission setting prevents you, reply exactly BLOCKED."

	run := func(mode adapter.Mode) bool {
		work := t.TempDir()
		if err := os.WriteFile(filepath.Join(work, "README.md"),
			[]byte("# demo\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		a := realAdapter(t, Options{MaxLiveProcesses: 2, IdleKill: time.Hour})
		sid, err := a.StartSession(ctx, adapter.StartOptions{
			Cwd: work, Mode: mode, Model: "haiku"})
		if err != nil {
			t.Fatalf("StartSession(%s): %v", mode, err)
		}
		ch, _ := a.Events(sid)
		if _, err := a.SendTurn(ctx, sid, ask, adapter.TurnOptions{}); err != nil {
			t.Fatal(err)
		}
		text := finalText(t, ch, 3*time.Minute)
		_, statErr := os.Stat(filepath.Join(work, "notes.txt"))
		t.Logf("mode=%s wrote=%v reply=%q", mode, statErr == nil, truncate(text, 80))
		return statErr == nil
	}

	// Without a control that actually writes, a blocked run proves
	// nothing: the model might simply have declined.
	if !run(adapter.ModeCode) {
		t.Skip("the control run did not write the file, so this test cannot " +
			"distinguish the permission mechanism from the model declining")
	}
	if run(adapter.ModeReview) {
		t.Fatal("review mode wrote a file; read-only is not enforced")
	}
}

// TestRealClaudePeerCannotUseTheHub asserts on init.tools, which names
// every tool the session can call. The masked server keeps its entry in
// mcp_servers and may still report `connected`, so connection status is
// not the signal; tool availability is.
func TestRealClaudePeerCannotUseTheHub(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the real provider test in short mode")
	}
	if os.Getenv("AGENT_HUB_TOKEN") == "" {
		t.Skip("AGENT_HUB_TOKEN is unset, so no process can reach the hub; " +
			"export it from ~/.local/share/agent-hub/token")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	ctx := context.Background()
	work := t.TempDir()

	hubTools := func(label string, mask, keepToken bool) int {
		t.Helper()
		o := SpawnOptions{Bin: bin, Cwd: work, Model: "haiku",
			PermissionMode: "plan", SessionID: NewSessionID()}
		if mask {
			o.MaskMCPServer = "agent-hub"
		}
		if keepToken {
			o.Env = os.Environ()
		}
		p, err := Spawn(ctx, o)
		if err != nil {
			t.Fatalf("Spawn(%s): %v", label, err)
		}
		defer p.Close()
		// init only arrives after the first message.
		if err := p.Send("Reply with exactly OK."); err != nil {
			t.Fatalf("Send(%s): %v", label, err)
		}
		deadline := time.After(120 * time.Second)
		seen := 0
		var kinds []string
		for p.Init() == nil {
			select {
			case m, open := <-p.Messages():
				if !open {
					t.Fatalf("%s exited before init after %d messages %v; stderr: %q",
						label, seen, kinds, p.Stderr())
				}
				seen++
				if len(kinds) < 8 {
					kinds = append(kinds, string(m.Type)+"/"+m.Subtype)
				}
			case <-p.Done():
				t.Fatalf("%s process ended after %d messages %v; stderr: %q",
					label, seen, kinds, p.Stderr())
			case <-deadline:
				t.Fatalf("no init for %s after %d messages %v; stderr: %q",
					label, seen, kinds, p.Stderr())
			}
		}
		n := 0
		for _, tool := range p.Init().Tools {
			if strings.HasPrefix(tool, "mcp__agent-hub__") {
				n++
			}
		}
		t.Logf("%s: hub tools=%d of %d", label, n, len(p.Init().Tools))
		return n
	}

	reference := hubTools("reference: no mask, token present", false, true)
	if reference == 0 {
		t.Skip("the hub exposes no tools even to a process that can reach it, " +
			"so this test cannot prove anything. Register agent-hub with " +
			"Claude and make sure the daemon is running.")
	}
	if n := hubTools("token strip only", false, false); n != 0 {
		t.Errorf("stripping the token alone left %d hub tools available", n)
	}
	if n := hubTools("production: mask and strip", true, false); n != 0 {
		t.Errorf("a masked peer still has %d hub tools available", n)
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
