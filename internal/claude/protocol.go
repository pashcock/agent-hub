// Package claude drives a native Claude Code agent through
// `claude -p --input-format stream-json --output-format stream-json`.
package claude

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type MsgType string

const (
	MsgSystem          MsgType = "system"
	MsgAssistant       MsgType = "assistant"
	MsgUser            MsgType = "user"
	MsgResult          MsgType = "result"
	MsgControlResponse MsgType = "control_response"
	MsgControlRequest  MsgType = "control_request"
	MsgRateLimit       MsgType = "rate_limit_event"
)

// SubtypeInterrupted is the result subtype of a turn that was stopped by
// a control_request. Measured, not assumed: the provider does emit a
// result for an interrupted turn.
const SubtypeInterrupted = "error_during_execution"

// InitInfo is the payload of the system/init line, which arrives only
// after the first user message.
type InitInfo struct {
	SessionID      string   `json:"session_id"`
	Model          string   `json:"model"`
	Cwd            string   `json:"cwd"`
	PermissionMode string   `json:"permissionMode"`
	Version        string   `json:"claude_code_version"`
	Tools          []string `json:"tools"`
	Skills         []string `json:"skills"`
	Agents         []string `json:"agents"`
	SlashCommands  []string `json:"slash_commands"`
	// MemoryPaths is an object keyed by scope, not a list. Getting this
	// wrong made the whole line fail to decode and left the adapter with
	// no capabilities at all, silently.
	MemoryPaths map[string]string `json:"memory_paths"`
	MCPServers  []MCPServerStatus `json:"mcp_servers"`
}

type MCPServerStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ResultUsage is the usage block of a result line. Claude reports a
// dollar figure as well as counts, which Codex does not.
type ResultUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// Message is one decoded stream-json line.
type Message struct {
	Type      MsgType
	Subtype   string
	SessionID string
	Result    string
	Text      string
	IsError   bool
	Init      *InitInfo
	RequestID string
	// Activity names the tool an assistant message invoked, if any.
	Activity string
	// Usage is set on a result line.
	Usage      *ResultUsage
	CostUSD    float64
	DurationMS int
	Model      string
	Raw        json.RawMessage
}

func decode(line []byte) Message {
	var head struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		SessionID string          `json:"session_id"`
		IsError   bool            `json:"is_error"`
		Result    string          `json:"result"`
		Message   json.RawMessage `json:"message"`
		Response  struct {
			RequestID string `json:"request_id"`
			Subtype   string `json:"subtype"`
		} `json:"response"`
		Usage        *ResultUsage        `json:"usage"`
		TotalCostUSD float64             `json:"total_cost_usd"`
		DurationMS   int                 `json:"duration_ms"`
		ModelUsage   map[string]struct{} `json:"-"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return Message{}
	}
	m := Message{
		Type: MsgType(head.Type), Subtype: head.Subtype,
		SessionID: head.SessionID, Result: head.Result,
		IsError: head.IsError, Raw: append(json.RawMessage(nil), line...),
	}
	switch m.Type {
	case MsgSystem:
		if head.Subtype == "init" {
			var info InitInfo
			err := json.Unmarshal(line, &info)
			// Keep whatever decoded. A field whose shape changes in a
			// future release must degrade the report, not blind the
			// adapter: without the session id there is nothing usable,
			// with it there is.
			if info.SessionID != "" {
				m.Init = &info
			} else if err == nil {
				m.Init = &info
			}
		}
	case MsgAssistant:
		m.Text = assistantText(head.Message)
		m.Activity = toolActivity(head.Message)
	case MsgControlResponse:
		m.RequestID = head.Response.RequestID
		m.Subtype = head.Response.Subtype
	case MsgResult:
		m.Usage = head.Usage
		m.CostUSD = head.TotalCostUSD
		m.DurationMS = head.DurationMS
		m.Model = dominantModel(line)
	}
	return m
}

// toolActivity names the tool an assistant message just invoked, with a
// compact argument, so a caller can see what a long turn is doing.
func toolActivity(raw json.RawMessage) string {
	var msg struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	for i := len(msg.Content) - 1; i >= 0; i-- {
		c := msg.Content[i]
		if c.Type != "tool_use" || c.Name == "" {
			continue
		}
		if arg := toolArg(c.Input); arg != "" {
			return c.Name + ": " + arg
		}
		return c.Name
	}
	return ""
}

// toolArg picks the one field of a tool's input worth showing.
func toolArg(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, key := range []string{"command", "file_path", "path", "pattern", "query", "url", "description"} {
		if v, ok := in[key].(string); ok && v != "" {
			return compact(v)
		}
	}
	return ""
}

func compact(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

func assistantText(raw json.RawMessage) string {
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	out := ""
	for _, c := range msg.Content {
		if c.Type == "text" && c.Text != "" {
			if out != "" {
				out += "\n"
			}
			out += c.Text
		}
	}
	return out
}

func userLine(text string) map[string]any {
	return map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
}

func interruptLine(requestID string) map[string]any {
	return map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    map[string]any{"subtype": "interrupt"},
	}
}

// NewSessionID returns a random UUIDv4 string. The hub names the session
// itself because system/init is too late to learn one from.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("claude: entropy unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("claude: entropy unavailable: %v", err))
	}
	return "req_" + hex.EncodeToString(b[:])
}

func jsonMarshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// dominantModel names the model that did most of the work in a turn.
//
// modelUsage is keyed by every model the turn touched, and a session with
// subagents touches several. Taking an arbitrary map key would report a
// different one on every run, so the biggest spender wins, falling back
// to token count when no cost is reported.
func dominantModel(line []byte) string {
	var mu struct {
		ModelUsage map[string]struct {
			CostUSD      float64 `json:"costUSD"`
			InputTokens  int     `json:"inputTokens"`
			OutputTokens int     `json:"outputTokens"`
		} `json:"modelUsage"`
	}
	if json.Unmarshal(line, &mu) != nil || len(mu.ModelUsage) == 0 {
		return ""
	}
	best, bestCost, bestTokens := "", -1.0, -1
	for name, u := range mu.ModelUsage {
		tokens := u.InputTokens + u.OutputTokens
		switch {
		case u.CostUSD > bestCost,
			u.CostUSD == bestCost && tokens > bestTokens,
			// Ties broken by name so the answer never depends on map order.
			u.CostUSD == bestCost && tokens == bestTokens && name < best:
			best, bestCost, bestTokens = name, u.CostUSD, tokens
		}
	}
	return best
}
