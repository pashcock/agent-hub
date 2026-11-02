package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pashcock/agent-hub/internal/adapter"
)

type usage struct {
	Input      int   `json:"inputTokens"`
	Output     int   `json:"outputTokens"`
	Total      int   `json:"totalTokens"`
	Cached     int   `json:"cachedReadTokens"`
	CacheWrite int   `json:"cacheCreationTokens"`
	Reasoning  int   `json:"reasoningTokens"`
	Duration   int   `json:"apiDurationMs"`
	CostTicks  int64 `json:"costUsdTicks"`
	Partial    bool  `json:"costIsPartial"`
	Incomplete bool  `json:"usageIsIncomplete"`
}

func (u usage) normalized(model string) *adapter.Usage {
	v := &adapter.Usage{Model: model, InputTokens: u.Input, OutputTokens: u.Output, TotalTokens: u.Total, CachedInput: u.Cached, CacheWrite: u.CacheWrite, ReasoningTokens: u.Reasoning, DurationMS: u.Duration}
	if !u.Partial && !u.Incomplete {
		v.CostUSD = float64(u.CostTicks) / 1e10
	}
	return v
}
func parseModels(raw json.RawMessage) ([]adapter.Model, error) {
	var env struct {
		Result json.RawMessage `json:"result"`
		Data   json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if len(env.Result) > 0 {
		raw = env.Result
	} else if len(env.Data) > 0 {
		raw = env.Data
	}
	var state struct {
		Available []struct {
			ID   string                     `json:"modelId"`
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"availableModels"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	out := []adapter.Model{}
	for _, m := range state.Available {
		if m.ID != "" {
			model := adapter.Model{ID: m.ID}
			_ = json.Unmarshal(m.Meta["reasoningEffort"], &model.DefaultEffort)
			var efforts []struct {
				Value   string `json:"value"`
				Default bool   `json:"default"`
			}
			if json.Unmarshal(m.Meta["reasoningEfforts"], &efforts) == nil {
				for _, e := range efforts {
					if e.Value != "" {
						model.Efforts = append(model.Efforts, e.Value)
						if model.DefaultEffort == "" && e.Default {
							model.DefaultEffort = e.Value
						}
					}
				}
			}
			out = append(out, model)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("grok: model catalog has no availableModels (unsupported protocol)")
	}
	return out, nil
}
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
			Type    string                      `json:"sessionUpdate"`
			Content struct{ Type, Text string } `json:"content"`
			Title   string                      `json:"title"`
			Status  string                      `json:"status"`
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
		if turn != "" && p.Update.Content.Type == "text" {
			s.text.WriteString(p.Update.Content.Text)
			ev.Kind = adapter.EventMessageDelta
			ev.Text = p.Update.Content.Text
		}
	case "tool_call", "tool_call_update":
		if turn != "" {
			ev.Kind = adapter.EventActivity
			ev.Activity = strings.TrimSpace(p.Update.Title + " " + p.Update.Status)
		}
	}
	s.q.put(ev)
}

type permissionParams struct {
	SessionID string             `json:"sessionId"`
	Options   []permissionOption `json:"options"`
	ToolCall  struct {
		Title     string          `json:"title"`
		Kind      string          `json:"kind"`
		RawInput  json.RawMessage `json:"rawInput"`
		Locations []struct {
			Path string `json:"path"`
		} `json:"locations"`
	} `json:"toolCall"`
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
	req.ID = "gp_" + s.id + "_" + string(id)
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
		return fmt.Errorf("grok: permission request is no longer pending")
	}
	if s.proc == nil {
		return errClosed
	}
	if d.Answer != "" {
		return fmt.Errorf("grok: free-text permission answers are unsupported")
	}
	kind := "reject_once"
	switch d.Verb {
	case "accept":
		kind = "allow_once"
	case "acceptForSession":
		kind = "allow_always"
	case "decline", "cancel":
	default:
		return fmt.Errorf("grok: invalid decision %q", d.Verb)
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
		return fmt.Errorf("grok: provider did not offer a one-time approval")
	}
	if err = s.proc.respond(p.wireID, map[string]any{"outcome": outcome}); err != nil {
		return err
	}
	delete(s.approvals, requestID)
	return nil
}
