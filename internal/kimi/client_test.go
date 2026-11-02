package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/pashcock/agent-hub/internal/adapter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type capture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *capture) Write(p []byte) (int, error) { c.mu.Lock(); defer c.mu.Unlock(); return c.b.Write(p) }
func (c *capture) Close() error                { return nil }
func (c *capture) String() string              { c.mu.Lock(); defer c.mu.Unlock(); return c.b.String() }
func clientHarness(t *testing.T, mode adapter.Mode) (*Adapter, *session, *capture) {
	a := fakeAdapter(t)
	output := &capture{}
	r := &rpc{in: output, done: make(chan struct{})}
	s := &session{id: "s", opts: adapter.StartOptions{Cwd: t.TempDir(), Mode: mode}, proc: r, turn: "turn", q: newQueue(), approvals: map[string]permission{}}
	a.sessions[s.id] = s
	t.Cleanup(func() { s.mu.Lock(); s.killTerminals(); s.proc = nil; s.mu.Unlock() })
	return a, s, output
}
func clientCall(a *Adapter, s *session, method string, p map[string]any) {
	p["sessionId"] = s.id
	b, _ := json.Marshal(p)
	a.clientRequest(s, s.proc, json.RawMessage(`1`), method, b)
}
func TestClientWorkspaceWriteUsesNativeApproval(t *testing.T) {
	a, s, out := clientHarness(t, adapter.ModeCode)
	path := filepath.Join(s.opts.Cwd, "test.txt")
	clientCall(a, s, "fs/write_text_file", map[string]any{"path": path, "content": "allowed"})
	if len(s.approvals) != 0 {
		t.Fatal("duplicate client approval")
	}
	data, e := os.ReadFile(path)
	if e != nil || string(data) != "allowed" {
		t.Fatalf("%s %v", data, e)
	}
	if !strings.Contains(out.String(), `"result":{}`) {
		t.Fatal(out.String())
	}
}
func TestClientDeclineAndReadOnly(t *testing.T) {
	for _, mode := range []adapter.Mode{adapter.ModeCode, adapter.ModeAsk} {
		t.Run(string(mode), func(t *testing.T) {
			a, s, out := clientHarness(t, mode)
			path := filepath.Join(t.TempDir(), "test.txt")
			clientCall(a, s, "fs/write_text_file", map[string]any{"path": path, "content": "forbidden"})
			if mode == adapter.ModeCode {
				ev := await(t, s.q.out, adapter.EventApproval)
				if e := a.Approve(context.Background(), s.id, ev.Approval.ID, adapter.Decision{Verb: "decline"}); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := os.Stat(path); !os.IsNotExist(e) {
				t.Fatal("declined write executed")
			}
			if !strings.Contains(out.String(), `"error"`) {
				t.Fatal(out.String())
			}
		})
	}
}
func TestClientWriteSymlinkGate(t *testing.T) {
	a, s, _ := clientHarness(t, adapter.ModeCode)
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(s.opts.Cwd, "link"))
	clientCall(a, s, "fs/write_text_file", map[string]any{"path": filepath.Join(s.opts.Cwd, "link", "file"), "content": "x"})
	ev := await(t, s.q.out, adapter.EventApproval)
	if ev.Approval.Paths[0] != filepath.Join(outside, "file") {
		t.Fatal(ev.Approval.Paths)
	}
}
func TestClientTerminalNativeApprovalAndCleanup(t *testing.T) {
	a, s, out := clientHarness(t, adapter.ModeCode)
	clientCall(a, s, "terminal/create", map[string]any{"command": "/bin/sh", "args": []string{"-c", "printf TERMINAL-OK"}, "cwd": s.opts.Cwd})
	if len(s.approvals) != 0 {
		t.Fatal("duplicate terminal approval")
	}
	var id string
	var term *terminal
	for id, term = range s.terminals {
	}
	select {
	case <-term.done:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal hung")
	}
	clientCall(a, s, "terminal/output", map[string]any{"terminalId": id})
	if !strings.Contains(out.String(), "TERMINAL-OK") {
		t.Fatal(out.String())
	}
	clientCall(a, s, "terminal/release", map[string]any{"terminalId": id})
	if len(s.terminals) != 0 {
		t.Fatal("terminal retained")
	}
}

func TestTerminalPolicyGates(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		env          []map[string]string
		class        adapter.ApprovalClass
	}{

		{"privileged", "cd /tmp && sudo id", nil, adapter.ClassPolicy},
		{"loader", "printf OK", []map[string]string{{"name": "LD_PRELOAD", "value": "/tmp/plugin.so"}}, adapter.ClassPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s, _ := clientHarness(t, adapter.ModeCode)
			clientCall(a, s, "terminal/create", map[string]any{"command": "/bin/bash", "args": []string{"-c", tc.script}, "env": tc.env})
			ev := await(t, s.q.out, adapter.EventApproval)
			if ev.Approval.Class != tc.class {
				t.Fatalf("%+v", ev.Approval)
			}
		})
	}
}
