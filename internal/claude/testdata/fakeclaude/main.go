// Command fakeclaude mimics `claude -p --input-format stream-json
// --output-format stream-json` closely enough to test the supervisor
// without spending tokens.
//
// It reproduces the four behaviours the adapter depends on:
//
//   - system/init is emitted only after the first user message
//   - --session-id is honoured, and --resume reuses it
//   - user messages are answered strictly one at a time, in order
//   - an interrupted turn still produces a result, with
//     subtype "error_during_execution" and is_error true
//
// Magic prompts: SLOW takes 30 seconds; CODEWORD echoes the first
// message this session ever saw.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	outMu    sync.Mutex
	out      = bufio.NewWriter(os.Stdout)
	session  string
	memo     string
	first    string
	sentInit bool

	work        = make(chan string, 64)
	interrupted atomic.Bool
)

func write(v any) {
	outMu.Lock()
	defer outMu.Unlock()
	b, _ := json.Marshal(v)
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
}

// flagValue reads --name value or --name=value out of the argv, so the
// fake does not need to model the real flag set.
func flagValue(name string) string {
	for i, a := range os.Args {
		if a == "--"+name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
		if strings.HasPrefix(a, "--"+name+"=") {
			return strings.TrimPrefix(a, "--"+name+"=")
		}
	}
	return ""
}

func main() {
	session = flagValue("session-id")
	if r := flagValue("resume"); r != "" {
		session = r
	}
	if session == "" {
		session = "fake-session"
	}
	memo = os.Getenv("FAKECLAUDE_MEMO")
	if memo != "" && flagValue("resume") != "" {
		if b, err := os.ReadFile(memo); err == nil {
			first = string(b)
		}
	}

	// One worker: turns are answered in order, never concurrently.
	go func() {
		for text := range work {
			handle(text)
		}
	}()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		var m struct {
			Type    string `json:"type"`
			Request struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			RequestID string `json:"request_id"`
			Message   struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "control_request":
			if m.Request.Subtype == "initialize" || m.Request.Subtype == "mcp_status" {
				if os.Getenv("FAKECLAUDE_CONTROL_MODE") == "hang" {
					continue
				}
				if os.Getenv("FAKECLAUDE_CONTROL_MODE") == "exit" {
					return
				}
				if os.Getenv("FAKECLAUDE_CONTROL_MODE") == "error" {
					write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": m.RequestID, "error": "unsupported control request"}})
					continue
				}
				var response any
				if m.Request.Subtype == "initialize" {
					response = map[string]any{"models": []any{map[string]any{"value": "opus", "resolvedModel": "claude-opus-5-5", "supportedEffortLevels": []string{"low", "medium", "high", "xhigh", "max"}}}}
					if path := os.Getenv("FAKECLAUDE_CATALOG"); path != "" {
						data, err := os.ReadFile(path)
						if err != nil {
							os.Exit(2)
						}
						response = json.RawMessage(data)
					}
				} else {
					response = map[string]any{"mcpServers": []any{map[string]string{"name": "agent-hub", "status": "connected"}, map[string]string{"name": "offline", "status": "failed"}}}
				}
				write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": m.RequestID, "response": response}})
				continue
			}
			if m.Request.Subtype == "interrupt" {
				interrupted.Store(true)
				write(map[string]any{"type": "control_response",
					"response": map[string]any{"subtype": "success",
						"request_id": m.RequestID,
						"response":   map[string]any{"still_queued": []string{}}}})
			}
		case "user":
			text := ""
			if len(m.Message.Content) > 0 {
				text = m.Message.Content[0].Text
			}
			emitInitOnce()
			work <- text
		}
	}
	close(work)
}

func emitInitOnce() {
	outMu.Lock()
	already := sentInit
	sentInit = true
	outMu.Unlock()
	if already {
		return
	}
	cwd, _ := os.Getwd()
	tools := []string{"Read", "Write", "Bash"}
	if !strings.Contains(strings.Join(os.Args, " "), "mcp__agent-hub") {
		tools = append(tools,
			"mcp__agent-hub__peer_start", "mcp__agent-hub__peer_poll")
	}
	write(map[string]any{
		"type": "system", "subtype": "init", "session_id": session,
		"model": flagValue("model"), "cwd": cwd,
		"permissionMode": flagValue("permission-mode"),
		"tools":          tools,
		"mcp_servers": []any{
			map[string]any{"name": "agent-hub", "status": "connected"}},
		"skills":       []string{"demo"},
		"agents":       []string{"reviewer"},
		"memory_paths": map[string]string{"auto": "/home/example/.claude/memory/"},
	})
}

func handle(text string) {
	outMu.Lock()
	if first == "" {
		first = text
		if memo != "" {
			_ = os.WriteFile(memo, []byte(text), 0o600)
		}
	}
	remembered := first
	outMu.Unlock()

	if strings.Contains(text, "SLOW") {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if interrupted.Load() {
				interrupted.Store(false)
				// An interrupted turn still reports a result.
				write(map[string]any{"type": "result",
					"subtype": "error_during_execution", "is_error": true,
					"session_id": session, "num_turns": 1, "result": ""})
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	reply := "ack: " + text
	if strings.Contains(text, "CODEWORD") {
		reply = remembered
	}
	write(map[string]any{"type": "assistant", "session_id": session,
		"message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": reply}}}})
	write(map[string]any{"type": "result", "subtype": "success",
		"is_error": false, "session_id": session, "num_turns": 1,
		"permission_denials": []any{}, "result": reply})
}
