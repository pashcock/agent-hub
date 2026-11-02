package kimi

import (
	"encoding/json"
	"testing"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func TestNativeApprovalUsesMatchingToolEvent(t *testing.T) {
	a, s, _ := clientHarness(t, adapter.ModeCode)
	// Real 2.3.2 shape: arguments on tool event; permission carries only ID/title.
	for _, call := range []struct{ id, command string }{{"turn:other", "printf harmless"}, {"turn:risky", "rm -rf guard-target"}} {
		raw, _ := json.Marshal(map[string]any{"sessionId": s.id, "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": call.id, "title": "Bash", "kind": "execute", "content": []map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": "tool summary"}}}, "rawInput": map[string]string{"command": call.command}}})
		a.notification(s, s.proc, "session/update", raw)
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": s.id, "toolCall": map[string]string{"toolCallId": "turn:risky", "title": "Bash"}, "options": []map[string]string{{"optionId": "reject", "kind": "reject_once"}}})
	a.request(s, s.proc, json.RawMessage(`"approval"`), "session/request_permission", raw)
	ev := await(t, s.q.out, adapter.EventApproval)
	if ev.Approval.Class != adapter.ClassCommand || ev.Approval.Command != "rm -rf guard-target" {
		t.Fatalf("lost native command: %+v", ev.Approval)
	}
}

func TestUnknownToolIDDoesNotBorrowAnotherCommand(t *testing.T) {
	p := permissionParams{ToolCall: permissionToolCall{ID: "missing", Title: "Bash"}}
	enrichPermission(&p, map[string]permissionToolCall{"other": {Kind: "execute", RawInput: json.RawMessage(`{"command":"printf harmless"}`)}})
	if req := classify(p); req.Class != adapter.ClassPolicy || req.Command != "" {
		t.Fatalf("guessed command: %+v", req)
	}
}

func TestNativeApprovalUsesCompleteStreamedArguments(t *testing.T) {
	a, s, _ := clientHarness(t, adapter.ModeCode)
	send := func(update map[string]any) {
		t.Helper()
		update["toolCallId"] = "1:risky"
		raw, _ := json.Marshal(map[string]any{"sessionId": s.id, "update": update})
		a.notification(s, s.proc, "session/update", raw)
	}
	send(map[string]any{"sessionUpdate": "tool_call", "title": "Bash", "kind": "execute", "status": "pending"})
	for _, args := range []string{`{"command":"rm`, `{"command":"rm -rf guard-target"}`} {
		send(map[string]any{"sessionUpdate": "tool_call_update", "status": "in_progress", "content": []map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": args}}}})
	}
	raw := json.RawMessage(`{"sessionId":"s","toolCall":{"toolCallId":"1:risky","title":"Bash"},"options":[{"optionId":"reject","kind":"reject_once"}]}`)
	a.request(s, s.proc, json.RawMessage(`17`), "session/request_permission", raw)
	ev := await(t, s.q.out, adapter.EventApproval)
	if ev.Approval.Command != "rm -rf guard-target" || ev.Approval.Class != adapter.ClassCommand {
		t.Fatalf("streamed args lost: %+v", ev.Approval)
	}
}
