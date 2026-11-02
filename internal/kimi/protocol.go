package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pashcock/agent-hub/internal/adapter"
)

// Kimi 2.1.1 emits context occupancy (usage_update.used/size), not turn
// consumption. Preserve those raw events; never turn them into token spend.
func (a *Adapter) notification(s *session, source *rpc, method string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.proc != source {
		return
	}
	turn := s.turn
	if method != "session/update" {
		s.q.put(adapter.Event{Kind: adapter.EventProviderRaw, TurnID: turn, Raw: raw})
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Type    string          `json:"sessionUpdate"`
			Content json.RawMessage `json:"content"`
			permissionToolCall
			Status string `json:"status"`
		} `json:"update"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if s.id != "" && p.SessionID != s.id {
		return
	}
	ev := adapter.Event{Kind: adapter.EventProviderRaw, TurnID: turn, Raw: raw}
	// Ignore replay text for the next turn: session/load emits historic chunks.
	switch p.Update.Type {
	case "agent_message_chunk":
		var content struct{ Type, Text string }
		if turn != "" && json.Unmarshal(p.Update.Content, &content) == nil && content.Type == "text" {
			s.text.WriteString(content.Text)
			ev.Kind = adapter.EventMessageDelta
			ev.Text = content.Text
		}
	case "tool_call", "tool_call_update":
		if turn != "" {
			if p.Update.ID != "" {
				if s.toolCalls == nil {
					s.toolCalls = map[string]permissionToolCall{}
				}
				call := s.toolCalls[p.Update.ID]
				call.ID = p.Update.ID
				if p.Update.Kind != "" {
					call.Kind = p.Update.Kind
				}
				if p.Update.Title != "" {
					call.Title = p.Update.Title
				}
				if len(p.Update.Locations) > 0 {
					call.Locations = p.Update.Locations
				}
				if len(p.Update.RawInput) > 0 {
					call.RawInput = p.Update.RawInput
				} else if p.Update.Status == "pending" || p.Update.Status == "in_progress" {
					// Permission can precede tool.call.started. Streaming updates
					// carry cumulative argument JSON as a text content block.
					var blocks []struct {
						Type    string
						Content struct{ Type, Text string }
					}
					if json.Unmarshal(p.Update.Content, &blocks) == nil {
						for _, block := range blocks {
							var args map[string]json.RawMessage
							if block.Type == "content" && block.Content.Type == "text" && json.Unmarshal([]byte(block.Content.Text), &args) == nil && args != nil {
								call.RawInput = json.RawMessage(block.Content.Text)
							}
						}
					}
				}
				s.toolCalls[p.Update.ID] = call
			}
			ev.Kind = adapter.EventActivity
			ev.Activity = strings.TrimSpace(p.Update.Title + " " + p.Update.Status)
		}
	}
	s.q.put(ev)
}

type permissionToolCall struct {
	ID        string          `json:"toolCallId"`
	Title     string          `json:"title"`
	Kind      string          `json:"kind"`
	RawInput  json.RawMessage `json:"rawInput"`
	Locations []struct {
		Path string `json:"path"`
	} `json:"locations"`
}

type permissionParams struct {
	SessionID string             `json:"sessionId"`
	Options   []permissionOption `json:"options"`
	ToolCall  permissionToolCall `json:"toolCall"`
}

// Kimi 2.3.x approval updates omit arguments; the preceding tool event holds
// the structured input. Never infer a shell command from display prose.
func enrichPermission(p *permissionParams, calls map[string]permissionToolCall) {
	if call, ok := calls[p.ToolCall.ID]; ok {
		if p.ToolCall.Kind == "" {
			p.ToolCall.Kind = call.Kind
		}
		if len(p.ToolCall.RawInput) == 0 {
			p.ToolCall.RawInput = call.RawInput
		}
		if len(p.ToolCall.Locations) == 0 {
			p.ToolCall.Locations = call.Locations
		}
	}
}

func classify(p permissionParams) *adapter.ApprovalRequest {
	req := &adapter.ApprovalRequest{Class: adapter.ClassPolicy, Reason: p.ToolCall.Title}
	switch p.ToolCall.Kind {
	case "execute":
		req.Class = adapter.ClassCommand
	case "edit", "delete", "move":
		req.Class = adapter.ClassFileChange
	}
	var input map[string]json.RawMessage
	_ = json.Unmarshal(p.ToolCall.RawInput, &input)
	for _, k := range []string{"command", "cmd"} {
		var command string
		if json.Unmarshal(input[k], &command) == nil && command != "" {
			req.Command = command
			break
		}
	}
	for _, l := range p.ToolCall.Locations {
		if l.Path != "" {
			req.Paths = append(req.Paths, l.Path)
		}
	}
	for _, k := range []string{"path", "file_path", "filePath"} {
		var p string
		if json.Unmarshal(input[k], &p) == nil && p != "" {
			req.Paths = append(req.Paths, p)
		}
	}
	// An unstructured execute title is not a reliable command for human-gating.
	if req.Class == adapter.ClassCommand && req.Command == "" {
		req.Class = adapter.ClassPolicy
	}
	if req.Class == adapter.ClassFileChange && len(req.Paths) == 0 {
		req.Class = adapter.ClassPolicy
	}
	return req
}
func (a *Adapter) request(s *session, source *rpc, id json.RawMessage, method string, raw json.RawMessage) {
	if a.clientRequest(s, source, id, method, raw) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.proc
	if r == nil || r != source {
		return
	}
	if method != "session/request_permission" {
		_ = r.reject(id, "agent-hub does not implement client method "+method)
		return
	}
	var p permissionParams
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID != s.id || s.turn == "" {
		_ = r.respond(id, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
		return
	}
	enrichPermission(&p, s.toolCalls)
	req := classify(p)
	// Read-only sessions cannot acquire additional write/execute privileges.
	if s.opts.Mode.ReadOnly() {
		s.denied = p.ToolCall.Title
		if s.denied == "" {
			s.denied = "tool requires additional permission"
		}
		s.q.put(adapter.Event{Kind: adapter.EventProviderRaw, TurnID: s.turn, Raw: raw})
		outcome := map[string]string{"outcome": "cancelled"}
		for _, o := range p.Options {
			if o.Kind == "reject_once" {
				outcome = map[string]string{"outcome": "selected", "optionId": o.ID}
				break
			}
		}
		_ = r.respond(id, map[string]any{"outcome": outcome})
		return
	}
	req.ID = "kp_" + s.id + "_" + string(id)
	req.Cwd = s.opts.Cwd
	req.Raw = raw
	s.approvals[req.ID] = permission{wireID: id, options: p.Options, class: req.Class}
	s.q.put(adapter.Event{Kind: adapter.EventApproval, TurnID: s.turn, Approval: req, Raw: raw})
}
func (a *Adapter) Approve(ctx context.Context, id, requestID string, d adapter.Decision) error {
	s, err := a.get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.approvals[requestID]
	if !ok {
		return fmt.Errorf("kimi: permission request is no longer pending")
	}
	if s.proc == nil {
		return errClosed
	}
	if d.Answer != "" {
		return fmt.Errorf("kimi: free-text permission answers are unsupported")
	}
	if p.action != nil {
		switch d.Verb {
		case "accept", "acceptForSession", "decline", "cancel":
		default:
			return fmt.Errorf("kimi: invalid decision %q", d.Verb)
		}
		delete(s.approvals, requestID)
		if !d.Accepted() {
			return s.proc.reject(p.wireID, "operation declined by agent-hub")
		}
		result, e := p.action()
		if e != nil {
			return s.proc.reject(p.wireID, e.Error())
		}
		return s.proc.respond(p.wireID, result)
	}
	kind := "reject_once"
	switch d.Verb {
	case "accept":
		kind = "allow_once"
	case "acceptForSession":
		kind = "allow_always"
	case "decline", "cancel":
	default:
		return fmt.Errorf("kimi: invalid decision %q", d.Verb)
	}
	// ACP allow_always can outlive a session depending on provider. Do not
	// silently grant it for the hub's session-scoped decision.
	if kind == "allow_always" {
		kind = "allow_once"
	}
	outcome := map[string]string{"outcome": "cancelled"}
	if d.Verb != "cancel" {
		for _, o := range p.options {
			if o.Kind == kind {
				outcome = map[string]string{"outcome": "selected", "optionId": o.ID}
				break
			}
		}
	}
	if d.Accepted() && outcome["outcome"] != "selected" {
		return fmt.Errorf("kimi: provider did not offer a one-time approval")
	}
	if err = s.proc.respond(p.wireID, map[string]any{"outcome": outcome}); err != nil {
		return err
	}
	delete(s.approvals, requestID)
	return nil
}
