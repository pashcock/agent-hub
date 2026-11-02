package kimi

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
	"github.com/pashcock/agent-hub/internal/adapter"
)

func TestMain(m *testing.M) {
	if os.Getenv("AGENT_HUB_KIMI_FAKE") == "1" {
		fakeACP()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func fakeACP() {
	sc := bufio.NewScanner(os.Stdin)
	send := func(v any) { b, _ := json.Marshal(v); os.Stdout.Write(append(b, '\n')) }
	sid := ""
	model := "test-kimi"
	effort := "low"
	mode := "default"
	var parked json.RawMessage
	config := func() []any {
		return []any{map[string]any{"id": "model", "currentValue": model, "options": []any{map[string]string{"value": "test-kimi"}}}, map[string]any{"id": "thinking", "currentValue": effort}, map[string]any{"id": "mode", "currentValue": mode}}
	}
	for sc.Scan() {
		var f frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		var p map[string]json.RawMessage
		json.Unmarshal(f.Params, &p)
		reply := func(v any) { send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": v}) }
		switch f.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]bool{"loadSession": true}})
		case "session/new":
			sid = uuid.NewString()
			reply(map[string]any{"sessionId": sid, "configOptions": config()})
		case "session/load":
			json.Unmarshal(p["sessionId"], &sid)
			reply(map[string]any{"configOptions": config()})
		case "session/set_config_option":
			var key, value string
			json.Unmarshal(p["configId"], &key)
			json.Unmarshal(p["value"], &value)
			if os.Getenv("AGENT_HUB_KIMI_WRONG_MODEL") != "1" {
				if key == "model" {
					model = value
				}
				if key == "thinking" {
					effort = value
				}
				if key == "mode" {
					mode = value
				}
			}
			reply(map[string]any{"configOptions": config()})
		case "session/close", "session/delete":
			reply(map[string]any{})
		case "session/prompt":
			var prompt []struct{ Text string }
			json.Unmarshal(p["prompt"], &prompt)
			text := prompt[0].Text
			if text == "die" {
				os.Exit(1)
			}
			if text == "wait" || text == "approve" {
				parked = f.ID
				if text == "approve" {
					send(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": sid, "toolCall": map[string]any{"kind": "execute", "title": "run tests", "rawInput": map[string]string{"command": "go test ./internal/kimi"}}, "options": []any{map[string]string{"optionId": "once", "kind": "allow_once"}, map[string]string{"optionId": "no", "kind": "reject_once"}}}})
				}
				continue
			}
			if strings.HasPrefix(text, "remember ") {
				os.WriteFile(filepath.Join(os.Getenv("KIMI_CODE_HOME"), sid+".txt"), []byte(text[9:]), 0600)
				text = "OK"
			} else if text == "recall" {
				b, _ := os.ReadFile(filepath.Join(os.Getenv("KIMI_CODE_HOME"), sid+".txt"))
				text = string(b)
			}
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sid, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}}}})
			reply(map[string]string{"stopReason": "end_turn"})
		case "session/cancel":
			if parked != nil {
				send(map[string]any{"jsonrpc": "2.0", "id": parked, "result": map[string]string{"stopReason": "cancelled"}})
				parked = nil
			}
		case "":
			if parked != nil {
				send(map[string]any{"jsonrpc": "2.0", "id": parked, "result": map[string]string{"stopReason": "end_turn"}})
				parked = nil
			}
		}
	}
}
func fakeAdapter(t *testing.T) *Adapter {
	t.Helper()
	t.Setenv("AGENT_HUB_KIMI_FAKE", "1")
	bin, _ := os.Executable()
	source := t.TempDir()
	err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("default_model='test-kimi'\n[models.test-kimi]\nmodel='native-kimi'\nsupport_efforts=['low','high']\ndefault_effort='low'\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAdapter(Options{Bin: bin, Home: t.TempDir(), SourceHome: source})
	t.Cleanup(func() { a.Close() })
	return a
}
func startFake(t *testing.T, a *Adapter, mode adapter.Mode) (string, <-chan adapter.Event) {
	t.Helper()
	id, e := a.StartSession(context.Background(), adapter.StartOptions{Cwd: t.TempDir(), Mode: mode, Model: "test-kimi", Effort: "low"})
	if e != nil {
		t.Fatal(e)
	}
	ch, e := a.Events(id)
	if e != nil {
		t.Fatal(e)
	}
	return id, ch
}
func await(t *testing.T, ch <-chan adapter.Event, kind adapter.EventKind) adapter.Event {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatal("closed before event")
			}
			if e.Kind == kind {
				return e
			}
		case <-timer.C:
			t.Fatalf("waiting for %s", kind)
		}
	}
}
func turn(t *testing.T, a *Adapter, id, text string) string {
	t.Helper()
	id, e := a.SendTurn(context.Background(), id, text, adapter.TurnOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func TestConversationResumeAndCatalog(t *testing.T) {
	a := fakeAdapter(t)
	caps, e := a.Capabilities(context.Background(), t.TempDir())
	if e != nil || len(caps.Models) != 1 || caps.Models[0].ResolvedModel != "native-kimi" {
		t.Fatalf("%+v %v", caps, e)
	}
	id, ch := startFake(t, a, adapter.ModeCode)
	turn(t, a, id, "remember CEDAR")
	await(t, ch, adapter.EventTurnCompleted)
	s, _ := a.get(id)
	s.mu.Lock()
	s.lastUsed = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	a.evictIdle()
	await(t, ch, adapter.EventSessionCold)
	turn(t, a, id, "recall")
	ev := await(t, ch, adapter.EventTurnCompleted)
	if ev.Text != "CEDAR" || ev.Usage != nil {
		t.Fatalf("%+v", ev)
	}
	// A fresh adapter can restore the same native id after daemon restart.
	opts := s.opts
	a.Close()
	b := NewAdapter(a.opts)
	defer b.Close()
	if e = b.ResumeSession(context.Background(), id, opts); e != nil {
		t.Fatal(e)
	}
	ch, _ = b.Events(id)
	turn(t, b, id, "recall")
	if ev = await(t, ch, adapter.EventTurnCompleted); ev.Text != "CEDAR" {
		t.Fatal(ev)
	}
}
func TestApprovalsAndReadOnly(t *testing.T) {
	for _, mode := range []adapter.Mode{adapter.ModeCode, adapter.ModeAsk} {
		t.Run(string(mode), func(t *testing.T) {
			a := fakeAdapter(t)
			id, ch := startFake(t, a, mode)
			turn(t, a, id, "approve")
			if mode == adapter.ModeCode {
				ev := await(t, ch, adapter.EventApproval)
				if ev.Approval.Class != adapter.ClassCommand || ev.Approval.Command == "" {
					t.Fatal(ev)
				}
				if e := a.Approve(context.Background(), id, ev.Approval.ID, adapter.Decision{Verb: "acceptForSession"}); e != nil {
					t.Fatal(e)
				}
				await(t, ch, adapter.EventTurnCompleted)
			} else {
				await(t, ch, adapter.EventTurnFailed)
			}
		})
	}
}
func TestCancelAndProcessDeath(t *testing.T) {
	a := fakeAdapter(t)
	id, ch := startFake(t, a, adapter.ModeCode)
	tid := turn(t, a, id, "wait")
	if e := a.Interrupt(context.Background(), id, tid); e != nil {
		t.Fatal(e)
	}
	await(t, ch, adapter.EventInterrupted)
	turn(t, a, id, "die")
	await(t, ch, adapter.EventTurnFailed)
	await(t, ch, adapter.EventSessionCold)
}
func TestModelMismatchAndCapacity(t *testing.T) {
	a := fakeAdapter(t)
	a.opts.MaxLiveProcesses = 1
	id, _ := startFake(t, a, adapter.ModeCode)
	if _, e := a.StartSession(context.Background(), adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeCode}); e == nil {
		t.Fatal("capacity ignored")
	}
	a.CloseSession(context.Background(), id)
	t.Setenv("AGENT_HUB_KIMI_WRONG_MODEL", "1")
	if _, e := a.StartSession(context.Background(), adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeCode, Model: "different"}); e == nil {
		t.Fatal("model silently substituted")
	}
}
func TestIsolation(t *testing.T) {
	a := fakeAdapter(t)
	t.Setenv("AGENT_HUB_TOKEN", "secret")
	t.Setenv("KIMI_MODEL_NAME", "override")
	o := adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeAsk}
	if e := a.prepare(o); e != nil {
		t.Fatal(e)
	}
	for _, kv := range a.environment(o) {
		if strings.HasPrefix(kv, "AGENT_HUB_TOKEN=") || strings.HasPrefix(kv, "KIMI_MODEL_NAME=") {
			t.Fatal("unsafe environment inherited")
		}
	}
	var c struct {
		Mode  string `toml:"default_permission_mode"`
		Tools struct{ Enabled, Disabled []string }
		Hooks []any
	}
	if _, e := toml.DecodeFile(filepath.Join(a.runtimeHome(o), "config.toml"), &c); e != nil {
		t.Fatal(e)
	}
	if c.Mode != "manual" || strings.Join(c.Tools.Enabled, ",") != "Read,Glob,Grep,WebSearch,FetchURL,select_tools,mcp__*__*" || len(c.Hooks) != 0 {
		t.Fatalf("unsafe profile %+v", c)
	}
	os.Mkdir(filepath.Join(o.Cwd, ".kimi-code"), 0700)
	os.WriteFile(filepath.Join(o.Cwd, ".kimi-code", "mcp.json"), []byte(`{}`), 0600)
	if e := a.prepare(o); e != nil {
		t.Fatal(e)
	}
}
func TestRejectSandboxAndInvalidMode(t *testing.T) {
	for _, o := range []adapter.StartOptions{{Cwd: "relative", Mode: adapter.ModeCode}, {Cwd: "/tmp", Mode: "bad"}, {Cwd: "/tmp", Mode: adapter.ModeCode, Sandbox: "workspace-write"}} {
		if validate(o) == nil {
			t.Fatal(o)
		}
	}
}
