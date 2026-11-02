package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

// The test binary acts as a deterministic ACP server in a separate process.
// This exercises framing, process lifetime and callbacks, without model calls.
func TestMain(m *testing.M) {
	if os.Getenv("AGENT_HUB_GROK_FAKE") == "1" {
		fakeACP()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func fakeACP() {
	control := os.Getenv("GROK_SANDBOX") == "off"
	sc := bufio.NewScanner(os.Stdin)
	var mu sync.Mutex
	send := func(v any) { mu.Lock(); defer mu.Unlock(); b, _ := json.Marshal(v); os.Stdout.Write(append(b, '\n')) }
	catalogCalls := 0
	var sid string
	var parked json.RawMessage
	for sc.Scan() {
		var f frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		var p map[string]json.RawMessage
		_ = json.Unmarshal(f.Params, &p)
		if trace := os.Getenv("AGENT_HUB_GROK_TRACE"); trace != "" {
			if file, err := os.OpenFile(trace, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err == nil {
				cwd, _ := os.Getwd()
				b, _ := json.Marshal(map[string]any{"method": f.Method, "control": control, "cwd": cwd, "home": os.Getenv("GROK_HOME"), "sandbox": os.Getenv("GROK_SANDBOX")})
				file.Write(append(b, '\n'))
				file.Close()
			}
		}
		reply := func(v any) { send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": v}) }
		switch f.Method {
		case "initialize":
			reply(map[string]any{"agentCapabilities": map[string]bool{"loadSession": true}})
		case "authenticate":
			if gate := os.Getenv("AGENT_HUB_GROK_AUTH_GATE"); control && gate != "" {
				for {
					if _, err := os.Stat(gate); err == nil {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			if control && os.Getenv("AGENT_HUB_GROK_AUTH_HANG") == "1" {
				continue
			}
			if control && os.Getenv("AGENT_HUB_GROK_AUTH_FAIL") == "1" {
				send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "error": map[string]any{"code": -32000, "message": "refresh unavailable"}})
				continue
			}
			if control && os.Getenv("AGENT_HUB_GROK_AUTH_STATE") != "" {
				os.WriteFile(os.Getenv("AGENT_HUB_GROK_AUTH_STATE"), []byte("renewed"), 0600)
			}
			reply(map[string]any{})
		case "_x.ai/plugins/list":
			reply(map[string]any{"result": map[string]any{"plugins": []any{}}})
		case "_x.ai/mcp/list":
			reply(map[string]any{"result": map[string]any{"servers": []any{}}})
		case "_x.ai/models/list":
			if control && os.Getenv("AGENT_HUB_GROK_CONTROL_DELAY") == "1" {
				catalogCalls++
				if catalogCalls < 3 {
					reply(map[string]any{"result": map[string]any{"availableModels": []any{map[string]string{"modelId": "fallback"}}}})
					continue
				}
			}
			if !control {
				catalogCalls++
				if os.Getenv("AGENT_HUB_GROK_DELAY_CATALOG") == "1" && catalogCalls < 3 {
					reply(map[string]any{"result": map[string]any{"availableModels": []any{map[string]string{"modelId": "fallback-grok"}}}})
					continue
				}
			}
			if control && os.Getenv("AGENT_HUB_GROK_NO_CATALOG") != "1" {
				cache := map[string]any{"fetched_at": time.Now(), "auth_method": "session", "models": map[string]any{"test-grok": map[string]any{"info": map[string]any{"hidden": false}}, "grok-4.7": map[string]any{"info": map[string]any{"hidden": false}}}}
				b, _ := json.Marshal(cache)
				os.WriteFile(filepath.Join(os.Getenv("GROK_HOME"), "models_cache.json"), b, 0600)
			}
			reply(map[string]any{"result": map[string]any{"availableModels": []any{map[string]string{"modelId": "test-grok"}, map[string]string{"modelId": "grok-4.7"}}}})
		case "session/new":
			if os.Getenv("AGENT_HUB_GROK_DELAY_CATALOG") == "1" && catalogCalls < 3 {
				var meta map[string]json.RawMessage
				json.Unmarshal(p["_meta"], &meta)
				json.Unmarshal(meta["sessionId"], &sid)
				reply(map[string]any{"sessionId": sid, "models": map[string]any{"currentModelId": "fallback-grok"}})
				continue
			}
			var meta map[string]json.RawMessage
			json.Unmarshal(p["_meta"], &meta)
			json.Unmarshal(meta["sessionId"], &sid)
			selected := "test-grok"
			if len(meta["modelId"]) > 0 {
				json.Unmarshal(meta["modelId"], &selected)
			}
			if os.Getenv("AGENT_HUB_GROK_WRONG_MODEL") == "1" {
				selected = "other-grok"
			}
			reply(map[string]any{"sessionId": sid, "models": map[string]any{"currentModelId": selected}})
		case "session/load":
			json.Unmarshal(p["sessionId"], &sid)
			reply(map[string]any{"models": map[string]any{"currentModelId": "test-grok"}})
		case "session/prompt":
			var prompt []struct{ Text string }
			json.Unmarshal(p["prompt"], &prompt)
			text := prompt[0].Text
			if text == "wait-for-renewed-auth" {
				id := append(json.RawMessage(nil), f.ID...)
				go func() {
					for {
						b, _ := os.ReadFile(os.Getenv("AGENT_HUB_GROK_AUTH_STATE"))
						if string(b) == "renewed" {
							send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]string{"stopReason": "end_turn"}})
							return
						}
						time.Sleep(5 * time.Millisecond)
					}
				}()
				continue
			}
			if text == "die" {
				os.Exit(1)
			}
			if text == "wait" {
				parked = f.ID
				continue
			}
			if text == "approve" {
				parked = f.ID
				send(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": sid, "toolCall": map[string]any{"title": "run tests", "kind": "execute", "rawInput": map[string]string{"command": "go test ./..."}}, "options": []any{map[string]string{"optionId": "once", "kind": "allow_once"}, map[string]string{"optionId": "no", "kind": "reject_once"}}}})
				continue
			}
			if strings.HasPrefix(text, "remember ") {
				os.WriteFile(filepath.Join(os.Getenv("GROK_HOME"), sid+".txt"), []byte(text[9:]), 0600)
				text = "OK"
			} else if text == "recall" {
				b, _ := os.ReadFile(filepath.Join(os.Getenv("GROK_HOME"), sid+".txt"))
				text = string(b)
			}
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sid, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}}}})
			reply(map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"modelId": "test-grok", "usage": map[string]any{"inputTokens": 100, "outputTokens": 5, "totalTokens": 105, "cachedReadTokens": 80, "costUsdTicks": 100000000}}})
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
	t.Setenv("AGENT_HUB_GROK_FAKE", "1")
	bin, _ := os.Executable()
	a := NewAdapter(Options{Bin: bin, Home: t.TempDir(), AuthPath: "/unused/auth.json"})
	t.Cleanup(func() { a.Close() })
	return a
}
func startFake(t *testing.T, a *Adapter) (string, <-chan adapter.Event) {
	t.Helper()
	id, e := a.StartSession(context.Background(), adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeCode, Model: "test-grok"})
	if e != nil {
		t.Fatal(e)
	}
	ch, _ := a.Events(id)
	return id, ch
}
func waitEvent(t *testing.T, ch <-chan adapter.Event, kinds ...adapter.EventKind) adapter.Event {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatal("event stream closed")
			}
			for _, k := range kinds {
				if e.Kind == k {
					return e
				}
			}
		case <-timer.C:
			t.Fatal("event timeout")
		}
	}
}
func sendFake(t *testing.T, a *Adapter, id, text string) string {
	t.Helper()
	turn, e := a.SendTurn(context.Background(), id, text, adapter.TurnOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return turn
}
func TestPersistentProcessAndRecovery(t *testing.T) {
	a := fakeAdapter(t)
	id, ch := startFake(t, a)
	s, _ := a.get(id)
	pid := s.proc.cmd.Process.Pid
	sendFake(t, a, id, "remember OLIVE")
	waitEvent(t, ch, adapter.EventTurnCompleted)
	sendFake(t, a, id, "recall")
	e := waitEvent(t, ch, adapter.EventTurnCompleted)
	if e.Text != "OLIVE" || s.proc.cmd.Process.Pid != pid {
		t.Fatalf("context/process lost: %+v", e)
	}
	if e.Usage == nil || e.Usage.TotalTokens != 105 || e.Usage.CostUSD != 0.01 {
		t.Fatalf("usage: %+v", e.Usage)
	}
	sendFake(t, a, id, "die")
	waitEvent(t, ch, adapter.EventTurnFailed)
	waitEvent(t, ch, adapter.EventSessionCold)
	sendFake(t, a, id, "recall")
	e = waitEvent(t, ch, adapter.EventTurnCompleted)
	if e.Text != "OLIVE" {
		t.Fatalf("resume: %q", e.Text)
	}
}
func TestCancelAndNextTurn(t *testing.T) {
	a := fakeAdapter(t)
	id, ch := startFake(t, a)
	turn := sendFake(t, a, id, "wait")
	waitEvent(t, ch, adapter.EventTurnStarted)
	if e := a.Interrupt(context.Background(), id, turn); e != nil {
		t.Fatal(e)
	}
	waitEvent(t, ch, adapter.EventInterrupted)
	sendFake(t, a, id, "next")
	if e := waitEvent(t, ch, adapter.EventTurnCompleted); e.Text != "next" {
		t.Fatal(e)
	}
}
func TestPermissionRoundTrip(t *testing.T) {
	a := fakeAdapter(t)
	id, ch := startFake(t, a)
	sendFake(t, a, id, "approve")
	e := waitEvent(t, ch, adapter.EventApproval)
	if e.Approval.Class != adapter.ClassCommand || e.Approval.Command != "go test ./..." {
		t.Fatal(e.Approval)
	}
	if err := a.Approve(context.Background(), id, e.Approval.ID, adapter.Decision{Verb: "accept"}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ch, adapter.EventTurnCompleted)
	if err := a.Approve(context.Background(), id, e.Approval.ID, adapter.Decision{Verb: "accept"}); err == nil {
		t.Fatal("replayed approval accepted")
	}
}
func TestUnknownPermissionIsHumanGated(t *testing.T) {
	for _, raw := range []string{`{"toolCall":{"kind":"execute","title":"sudo something"}}`, `{"toolCall":{"kind":"edit"}}`, `{"toolCall":{"kind":"other"}}`} {
		var p permissionParams
		json.Unmarshal([]byte(raw), &p)
		if classify(p).Class != adapter.ClassPolicy {
			t.Fatal(raw)
		}
	}
}
func TestChildEnvironment(t *testing.T) {
	a := fakeAdapter(t)
	t.Setenv("AGENT_HUB_TOKEN", "not-a-real-secret")
	t.Setenv("XAI_API_KEY", "not-a-real-key")
	env := strings.Join(a.environment(adapter.StartOptions{Mode: adapter.ModeReview}), "\n")
	for _, bad := range []string{"AGENT_HUB_TOKEN=", "XAI_API_KEY="} {
		if strings.Contains(env, bad) {
			t.Fatal("child inherits " + bad)
		}
	}
	if !strings.Contains(env, "GROK_SANDBOX=read-only") || !strings.Contains(env, "GROK_CLAUDE_MCPS_ENABLED=false") {
		t.Fatal("missing isolation")
	}
}
func TestQueueCloseWithoutReader(t *testing.T) {
	q := newQueue()
	for range 1024 {
		q.put(adapter.Event{Kind: adapter.EventTurnCompleted})
	}
	q.close()
	for range q.out {
	}
}

func TestModelMismatchRejected(t *testing.T) {
	t.Setenv("AGENT_HUB_GROK_WRONG_MODEL", "1")
	a := fakeAdapter(t)
	_, err := a.StartSession(context.Background(), adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeCode, Model: "test-grok"})
	if err == nil {
		t.Fatal("silently accepted a different model")
	}
}

func TestIncompleteCostNotReported(t *testing.T) {
	for _, u := range []usage{{CostTicks: 100000000, Partial: true}, {CostTicks: 100000000, Incomplete: true}} {
		if got := u.normalized("test-grok"); got.CostUSD != 0 {
			t.Fatalf("incomplete cost reported: %+v", got)
		}
	}
}

func TestRawTokenMetadataIsNotUsage(t *testing.T) {
	a := &Adapter{}
	proc := &rpc{}
	s := &session{id: "session", turn: "turn", proc: proc, q: newQueue()}
	defer s.q.close()
	raw := json.RawMessage(`{"sessionId":"session","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"diagnostic"},"_meta":{"totalTokens":44441}}}`)
	for i := 0; i < 3; i++ {
		a.notification(s, proc, "session/update", raw)
		event := waitEvent(t, s.q.out, adapter.EventProviderRaw)
		if event.Usage != nil {
			t.Fatal("raw counters treated as new usage")
		}
	}
}

func TestSessionWaitsForItsOwnCatalog(t *testing.T) {
	t.Setenv("AGENT_HUB_GROK_DELAY_CATALOG", "1")
	a := fakeAdapter(t)
	id, _ := startFake(t, a)
	if err := a.CloseSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if used, _ := a.Capacity(); used != 0 {
		t.Fatalf("retained %d slots", used)
	}
}

func TestMissingSessionModelCancellationReleasesSlot(t *testing.T) {
	a := fakeAdapter(t)
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("AGENT_HUB_GROK_TRACE", trace)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := a.StartSession(ctx, adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeReview, Model: "unavailable-model"})
	if err == nil || !strings.Contains(err.Error(), "unavailable-model") {
		t.Fatalf("unexpected error: %v", err)
	}
	if used, _ := a.Capacity(); used != 0 {
		t.Fatalf("retained %d slots", used)
	}
	data, _ := os.ReadFile(trace)
	if strings.Contains(string(data), "session/new") || strings.Contains(string(data), "session/prompt") {
		t.Fatal("created a session before the requested model was ready")
	}
}

func TestNewSessionDefaultsToGrok47(t *testing.T) {
	a := fakeAdapter(t)
	id, err := a.StartSession(context.Background(), adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeReview})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := a.get(id)
	if s.opts.Model != "grok-4.7" {
		t.Fatalf("default model=%q", s.opts.Model)
	}
}
