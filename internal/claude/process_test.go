package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func buildFakeClaude(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakeclaude")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeclaude")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fakeclaude: %v\n%s", err, out)
	}
	return bin
}

func spawnFake(t *testing.T, o SpawnOptions) *Process {
	t.Helper()
	if o.Bin == "" {
		o.Bin = buildFakeClaude(t)
	}
	if o.Cwd == "" {
		o.Cwd = t.TempDir()
	}
	if o.SessionID == "" && o.ResumeSessionID == "" {
		o.SessionID = NewSessionID()
	}
	p, err := Spawn(context.Background(), o)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func waitResult(t *testing.T, p *Process, within time.Duration) Message {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m, open := <-p.Messages():
			if !open {
				t.Fatal("the message stream closed before a result arrived")
			}
			if m.Type == MsgResult {
				return m
			}
		case <-deadline:
			t.Fatal("timed out waiting for a result")
		}
	}
}

func TestInitArrivesOnlyAfterTheFirstMessage(t *testing.T) {
	p := spawnFake(t, SpawnOptions{})
	// The provider says nothing until it is asked something, so an
	// adapter must never block on init before sending.
	select {
	case m := <-p.Messages():
		t.Fatalf("a message arrived before any input: %+v", m)
	case <-time.After(time.Second):
	}
	if err := p.Send("hello"); err != nil {
		t.Fatal(err)
	}
	waitResult(t, p, 15*time.Second)
	if p.Init() == nil {
		t.Fatal("Init must be populated once system/init has arrived")
	}
}

func TestSessionIDIsChosenByTheCaller(t *testing.T) {
	want := NewSessionID()
	p := spawnFake(t, SpawnOptions{SessionID: want})
	p.Send("hi")
	r := waitResult(t, p, 15*time.Second)
	if r.SessionID != want {
		t.Errorf("result session = %q, want the id we chose %q", r.SessionID, want)
	}
	if p.Init().SessionID != want {
		t.Errorf("init session = %q, want %q", p.Init().SessionID, want)
	}
}

func TestTurnsAreAnsweredOneAtATimeInOrder(t *testing.T) {
	p := spawnFake(t, SpawnOptions{})
	// The second message is fast, the first is slow. A provider that
	// answered them concurrently would return them out of order.
	p.Send("SLOW first")
	time.Sleep(300 * time.Millisecond)
	p.Send("second")

	r1 := waitResult(t, p, 60*time.Second)
	r2 := waitResult(t, p, 60*time.Second)
	if !strings.Contains(r1.Result, "SLOW first") {
		t.Errorf("first result = %q, want the slow turn to finish first", r1.Result)
	}
	if !strings.Contains(r2.Result, "second") {
		t.Errorf("second result = %q", r2.Result)
	}
}

func TestInterruptedTurnStillProducesAResult(t *testing.T) {
	p := spawnFake(t, SpawnOptions{})
	p.Send("SLOW work")
	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	r := waitResult(t, p, 20*time.Second)
	if !r.IsError || r.Subtype != "error_during_execution" {
		t.Fatalf("interrupted result = %+v, want is_error with error_during_execution", r)
	}
	// The process must still take work afterwards.
	if err := p.Send("after"); err != nil {
		t.Fatalf("Send after interrupt: %v", err)
	}
	waitResult(t, p, 20*time.Second)
}

func TestResumeCarriesTheSessionForward(t *testing.T) {
	bin := buildFakeClaude(t)
	dir := t.TempDir()
	memo := filepath.Join(dir, "memo")
	sid := NewSessionID()
	env := append(os.Environ(), "FAKECLAUDE_MEMO="+memo)

	p1 := spawnFake(t, SpawnOptions{Bin: bin, Cwd: dir, SessionID: sid, Env: env})
	p1.Send("remember PAPAYA")
	waitResult(t, p1, 15*time.Second)
	p1.Close()

	p2 := spawnFake(t, SpawnOptions{Bin: bin, Cwd: dir, ResumeSessionID: sid, Env: env})
	p2.Send("CODEWORD")
	r := waitResult(t, p2, 15*time.Second)
	if r.Result != "remember PAPAYA" {
		t.Errorf("resume lost the context: %q", r.Result)
	}
}

func TestCloseWhileTheReaderIsBusyDoesNotPanic(t *testing.T) {
	// Close used to close the message channel itself, which could race a
	// send from the read loop and panic. Hammer that window.
	for i := 0; i < 20; i++ {
		p := spawnFake(t, SpawnOptions{})
		p.Send("chatter")
		time.Sleep(time.Duration(i) * time.Millisecond)
		p.Close()
	}
}

func TestBuildArgsCarriesTheMaskAndModeFlags(t *testing.T) {
	got := BuildArgs(SpawnOptions{
		Cwd: "/tmp/p", SessionID: "11111111-2222-3333-4444-555555555555",
		Model: "opus", Effort: "high", PermissionMode: "plan",
		DisallowedTools: []string{"Edit", "Write"}, MaskMCPServer: "agent-hub",
	})
	for _, pair := range [][2]string{
		{"--session-id", "11111111-2222-3333-4444-555555555555"},
		{"--model", "opus"}, {"--effort", "high"},
		{"--permission-mode", "plan"},
		{"--input-format", "stream-json"}, {"--output-format", "stream-json"},
	} {
		i := slices.Index(got, pair[0])
		if i < 0 || i+1 >= len(got) || got[i+1] != pair[1] {
			t.Errorf("args missing %s %s; got %v", pair[0], pair[1], got)
		}
	}
	if !slices.Contains(got, "-p") || !slices.Contains(got, "--verbose") {
		t.Errorf("args = %v, want -p and --verbose", got)
	}
	i := slices.Index(got, "--disallowedTools")
	if i < 0 {
		t.Fatalf("no --disallowedTools in %v", got)
	}
	for _, want := range []string{"mcp__agent-hub", "Edit", "Write"} {
		if !strings.Contains(got[i+1], want) {
			t.Errorf("--disallowedTools %q is missing %q", got[i+1], want)
		}
	}
	if slices.Contains(got, "--strict-mcp-config") {
		t.Error("--strict-mcp-config would discard the project's own MCP servers")
	}
}

func TestBuildArgsResumeReplacesSessionID(t *testing.T) {
	got := BuildArgs(SpawnOptions{Cwd: "/tmp/p", SessionID: "new", ResumeSessionID: "old"})
	if i := slices.Index(got, "--resume"); i < 0 || got[i+1] != "old" {
		t.Fatalf("args = %v, want --resume old", got)
	}
	if slices.Contains(got, "--session-id") {
		t.Error("--session-id and --resume must not both be passed")
	}
}
