// Command fakeserver mimics the parts of `codex app-server` that the
// JSON-RPC client needs, so client tests need no real binary.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

type msg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

var (
	mu                sync.Mutex
	out               = bufio.NewWriter(os.Stdout)
	pending           = map[string]chan json.RawMessage{}
	lifecycleCalls    []string
	unsubscribeStatus = "unsubscribed"
	unsubscribeFail   bool
)

func write(v any) {
	mu.Lock()
	defer mu.Unlock()
	b, _ := json.Marshal(v)
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		var m msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		// A response to a request we sent.
		if m.Method == "" && len(m.ID) > 0 {
			mu.Lock()
			ch, ok := pending[string(m.ID)]
			mu.Unlock()
			if ok {
				ch <- m.Result
			}
			continue
		}
		go handle(m)
	}
}

func handle(m msg) {
	switch m.Method {
	case "echo":
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID), "result": json.RawMessage(m.Params)})
	case "boom":
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
			"error": map[string]any{"code": -32000, "message": "boom happened"}})
	case "notify":
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID), "result": map[string]any{}})
		write(map[string]any{"jsonrpc": "2.0", "method": "thing/happened",
			"params": map[string]any{"detail": "hello"}})
	case "ask":
		reqID := json.RawMessage(`"srv-1"`)
		ch := make(chan json.RawMessage, 1)
		mu.Lock()
		pending[string(reqID)] = ch
		mu.Unlock()
		write(map[string]any{"jsonrpc": "2.0", "id": reqID, "method": "needApproval",
			"params": map[string]any{"command": "rm -rf /"}})
		got := <-ch
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
			"result": map[string]any{"decisionEcho": json.RawMessage(got)}})
	case "initialize":
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
			"result": map[string]any{"ok": true}})
	case "thread/start":
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
			"result": map[string]any{"thread": map[string]any{"id": "thread-1"}}})
	case "thread/unsubscribe", "thread/resume", "test/lifecycle":
		mu.Lock()
		var result any = map[string]any{}
		fail := false
		if m.Method == "test/lifecycle" {
			var p struct {
				Fail   bool   `json:"fail"`
				Status string `json:"status"`
			}
			json.Unmarshal(m.Params, &p)
			unsubscribeFail = p.Fail
			if p.Status != "" {
				unsubscribeStatus = p.Status
			}
			result = append([]string{}, lifecycleCalls...)
		} else {
			lifecycleCalls = append(lifecycleCalls, m.Method)
			if m.Method == "thread/unsubscribe" {
				result = map[string]any{"status": unsubscribeStatus}
				fail = unsubscribeFail
			}
		}
		mu.Unlock()
		if fail {
			write(map[string]any{"id": rawOrNull(m.ID), "error": map[string]any{"code": -32000, "message": "unsubscribe failed"}})
		} else {
			write(map[string]any{"id": rawOrNull(m.ID), "result": result})
		}
	case "turn/start":
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
			"result": map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress"}}})
		write(map[string]any{"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"threadId": "thread-1",
				"turn": map[string]any{"id": "turn-1", "status": "inProgress"}}})
		write(map[string]any{"jsonrpc": "2.0", "method": "item/agentMessage/delta",
			"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "delta": "par"}})
		write(map[string]any{"jsonrpc": "2.0", "method": "item/agentMessage/delta",
			"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "delta": "tial"}})
		write(map[string]any{"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{"threadId": "thread-1",
				"turn": map[string]any{"id": "turn-1", "status": "completed",
					"items": []any{map[string]any{"type": "agentMessage", "id": "m1", "text": "FINAL"}}}}})
	case "model/list":
		var params struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(m.Params, &params)
		if params.Cursor != "" {
			if os.Getenv("FAKE_CODEX_MODEL_PAGE_ERROR") == "1" {
				write(map[string]any{"id": rawOrNull(m.ID), "error": map[string]any{"code": -32000, "message": "catalog unavailable"}})
				return
			}
			var next any
			if os.Getenv("FAKE_CODEX_MODEL_CURSOR_LOOP") == "1" {
				next = "page-2"
			}
			write(map[string]any{"id": rawOrNull(m.ID), "result": map[string]any{
				"data": []any{
					map[string]any{"id": "picker-sol", "model": "gpt-6-sol", "defaultReasoningEffort": "medium", "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "high"}, map[string]string{"reasoningEffort": "ultra"}}},
					map[string]any{"id": "gpt-6-luna", "defaultReasoningEffort": "medium"},
					map[string]any{"id": "hidden-model", "hidden": true},
				}, "nextCursor": next}})
			return
		}
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
			"result": map[string]any{"data": []any{map[string]any{
				"id": "gpt-5.6-luna", "defaultReasoningEffort": "medium",
				"supportedReasoningEfforts": []any{
					map[string]any{"reasoningEffort": "low"},
					map[string]any{"reasoningEffort": "high"}},
				"hidden": false}}, "nextCursor": "page-2"}})
	case "mcpServerStatus/list":
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
			"result": map[string]any{"data": []any{
				map[string]any{"name": "node_repl", "tools": map[string]any{"js": map[string]any{}}}}}})
	case "turn/interrupt":
		mu.Lock()
		lifecycleCalls = append(lifecycleCalls, m.Method)
		mu.Unlock()
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID), "result": map[string]any{}})
		write(map[string]any{"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{"threadId": "thread-1",
				"turn": map[string]any{"id": "turn-1", "status": "interrupted"}}})
	case "turn/steer":
		write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID), "result": map[string]any{}})
	case "provoke/approval":
		ask(m, json.RawMessage(`"approval-1"`), "item/commandExecution/requestApproval",
			map[string]any{"threadId": "thread-1", "turnId": "turn-1",
				"itemId": "item-9", "command": "sudo rm -rf /", "cwd": "/tmp/p",
				"reason": "cleanup"})
	case "provoke/permissions":
		ask(m, json.RawMessage(`"approval-2"`), "item/permissions/requestApproval",
			map[string]any{"threadId": "thread-1", "turnId": "turn-1", "cwd": "/tmp/p",
				"reason": "needs the repo", "permissions": map[string]any{
					"fileSystem": map[string]any{"entries": []any{
						map[string]any{"access": "read", "path": "/tmp/p"}}}}})
	case "provoke/userinput":
		ask(m, json.RawMessage(`"approval-3"`), "item/tool/requestUserInput",
			map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "item-3",
				"isBlocking": true, "questions": []any{
					map[string]any{"id": "q1", "header": "Deploy", "question": "Which env?"}}})
	case "provoke/elicitation":
		ask(m, json.RawMessage(`"approval-4"`), "mcpServer/elicitation/request",
			map[string]any{"threadId": "thread-1", "turnId": "turn-1",
				"message": "please confirm"})
	case "provoke/unknown":
		ask(m, json.RawMessage(`"approval-5"`), "some/unknown/request",
			map[string]any{"threadId": "thread-1"})
	default:
		if len(m.ID) > 0 {
			write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
				"error": map[string]any{"code": -32601, "message": fmt.Sprintf("no method %q", m.Method)}})
		}
	}
}

// ask sends a server-to-client request, waits for the client's response,
// and then answers the original call with whatever came back.
func ask(m msg, reqID json.RawMessage, method string, params map[string]any) {
	ch := make(chan json.RawMessage, 1)
	mu.Lock()
	pending[string(reqID)] = ch
	mu.Unlock()
	write(map[string]any{"jsonrpc": "2.0", "id": reqID, "method": method, "params": params})
	got := <-ch
	write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(m.ID),
		"result": map[string]any{"decisionEcho": json.RawMessage(got)}})
}

func rawOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}
