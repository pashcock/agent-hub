package grok

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func TestCloseDuringResumeDoesNotResurrectOrLeakProcess(t *testing.T) {
	a := fakeAdapter(t)
	id, ch := startFake(t, a)
	sendFake(t, a, id, "hello")
	waitEvent(t, ch, adapter.EventTurnCompleted)
	s, _ := a.get(id)
	s.mu.Lock()
	s.lastUsed = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	a.evictIdle()
	waitEvent(t, ch, adapter.EventSessionCold)
	root := t.TempDir()
	gate, trace := filepath.Join(root, "gate"), filepath.Join(root, "trace")
	t.Setenv("AGENT_HUB_GROK_AUTH_GATE", gate)
	t.Setenv("AGENT_HUB_GROK_TRACE", trace)
	a.controlMu.Lock()
	a.lastAuth = time.Time{}
	a.controlMu.Unlock()
	resumed := make(chan error, 1)
	go func() { resumed <- a.ResumeSession(context.Background(), id, s.opts) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(trace)
		if strings.Contains(string(b), `"method":"authenticate"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resume did not reach auth gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := a.CloseSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-resumed:
		if err == nil {
			t.Error("closed session resurrected by in-flight resume")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not exit")
	}
	if used, _ := a.Capacity(); used != 0 {
		t.Errorf("closed session still occupies %d slots", used)
	}
}

func TestCloseCompletedSessionReapsProcessAndReleasesSlot(t *testing.T) {
	a := fakeAdapter(t)
	id, ch := startFake(t, a)
	sendFake(t, a, id, "hello")
	waitEvent(t, ch, adapter.EventTurnCompleted)
	if used, _ := a.Capacity(); used != 1 {
		t.Fatalf("completed turn capacity=%d", used)
	}
	s, _ := a.get(id)
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if err := a.CloseSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if used, _ := a.Capacity(); used != 0 {
		t.Fatalf("closed session capacity=%d", used)
	}
	select {
	case <-proc.reaped:
	default:
		t.Fatal("close returned before reaping the child")
	}
}

func TestIdleEvictionResumesSavedContext(t *testing.T) {
	a := fakeAdapter(t)
	id, ch := startFake(t, a)
	sendFake(t, a, id, "remember OLIVE")
	waitEvent(t, ch, adapter.EventTurnCompleted)
	s, _ := a.get(id)
	s.mu.Lock()
	pid := s.proc.cmd.Process.Pid
	s.lastUsed = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	a.evictIdle()
	waitEvent(t, ch, adapter.EventSessionCold)
	if used, _ := a.Capacity(); used != 0 {
		t.Fatalf("process slot retained: %d", used)
	}
	sendFake(t, a, id, "recall")
	ev := waitEvent(t, ch, adapter.EventTurnCompleted)
	if ev.Text != "OLIVE" {
		t.Fatalf("lost native context: %q", ev.Text)
	}
	s.mu.Lock()
	nextPID := s.proc.cmd.Process.Pid
	s.mu.Unlock()
	if pid == nextPID {
		t.Fatal("process did not restart")
	}
}
func TestIdleEvictionNeverKillsRunningOrParkedTurn(t *testing.T) {
	for _, prompt := range []string{"wait", "approve"} {
		t.Run(prompt, func(t *testing.T) {
			a := fakeAdapter(t)
			id, ch := startFake(t, a)
			turn := sendFake(t, a, id, prompt)
			if prompt == "approve" {
				waitEvent(t, ch, adapter.EventApproval)
			} else {
				waitEvent(t, ch, adapter.EventTurnStarted)
			}
			s, _ := a.get(id)
			s.mu.Lock()
			s.lastUsed = time.Now().Add(-time.Hour)
			pid := s.proc.cmd.Process.Pid
			s.mu.Unlock()
			a.evictIdle()
			s.mu.Lock()
			live := s.proc != nil && s.proc.cmd.Process.Pid == pid
			s.mu.Unlock()
			if !live {
				t.Fatal("active process evicted")
			}
			if err := a.Interrupt(context.Background(), id, turn); err != nil {
				t.Fatal(err)
			}
			waitEvent(t, ch, adapter.EventInterrupted, adapter.EventTurnCompleted)
		})
	}
}
func TestIdleSweeperReleasesWithoutTraffic(t *testing.T) {
	t.Setenv("AGENT_HUB_GROK_FAKE", "1")
	bin, _ := os.Executable()
	a := NewAdapter(Options{Bin: bin, Home: t.TempDir(), AuthPath: "/unused/auth.json", IdleKill: 100 * time.Millisecond})
	defer a.Close()
	id, ch := startFake(t, a)
	sendFake(t, a, id, "hello")
	waitEvent(t, ch, adapter.EventTurnCompleted)
	waitEvent(t, ch, adapter.EventSessionCold)
	if used, _ := a.Capacity(); used != 0 {
		t.Fatalf("still live: %d", used)
	}
}

func TestResumeKeepsExplicitSandboxWhenCallerOmitsIt(t *testing.T) {
	a := fakeAdapter(t)
	opts := adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeCode, Model: "test-grok", Sandbox: "workspace-write"}
	id, err := a.StartSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := a.Events(id)
	sendFake(t, a, id, "remember PINNED")
	waitEvent(t, ch, adapter.EventTurnCompleted)
	s, _ := a.get(id)
	s.mu.Lock()
	s.lastUsed = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	a.evictIdle()
	waitEvent(t, ch, adapter.EventSessionCold)
	opts.Sandbox = ""
	if err = a.ResumeSession(context.Background(), id, opts); err != nil {
		t.Fatal(err)
	}
	sendFake(t, a, id, "recall")
	if e := waitEvent(t, ch, adapter.EventTurnCompleted); e.Text != "PINNED" {
		t.Fatal("context lost")
	}
}
