package kimi

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"github.com/pashcock/agent-hub/internal/adapter"
)

// Native Kimi checks routine operations before ACP execution. The client retains
// read-only enforcement and approval for outside writes and policy changes.
type terminal struct {
	cmd      *exec.Cmd
	output   terminalOutput
	done     chan struct{}
	exitCode int
}

var privilegeCommand = regexp.MustCompile(`(^|[\s;&|()])(?:/[^\s;&|()]*/)?(?:sudo|su|pkexec|doas)(?:[\s;&|()]|$)`)

func ordinaryTerminalEnv(name, value string) bool {
	switch name {
	case "NO_COLOR":
		return value == "1"
	case "TERM":
		return value == "dumb"
	case "GIT_TERMINAL_PROMPT":
		return value == "0"
	case "SHELL":
		return value == "/bin/bash" || value == "/bin/sh"
	}
	return false
}

type terminalOutput struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (o *terminalOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	o.data = append(o.data, p...)
	if len(o.data) > o.limit {
		o.data = append([]byte(nil), o.data[len(o.data)-o.limit:]...)
		o.truncated = true
	}
	return n, nil
}
func (t *terminal) kill() {
	select {
	case <-t.done:
		return
	default:
	}
	_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
}
func (s *session) killTerminals() {
	for _, t := range s.terminals {
		t.kill()
	}
	s.terminals = map[string]*terminal{}
}

func resolvedPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("kimi: ACP file paths must be absolute")
	}
	p := filepath.Clean(path)
	suffix := []string{}
	for {
		resolved, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", e
		}
		suffix = append(suffix, filepath.Base(p))
		p = parent
	}
}

func (a *Adapter) clientRequest(s *session, r *rpc, id json.RawMessage, method string, raw json.RawMessage) bool {
	if !strings.HasPrefix(method, "fs/") && !strings.HasPrefix(method, "terminal/") {
		return false
	}
	var p struct {
		SessionID       string                         `json:"sessionId"`
		Path            string                         `json:"path"`
		Content         string                         `json:"content"`
		Line            int                            `json:"line"`
		Limit           int                            `json:"limit"`
		Command         string                         `json:"command"`
		Args            []string                       `json:"args"`
		Cwd             string                         `json:"cwd"`
		Env             []struct{ Name, Value string } `json:"env"`
		OutputByteLimit int                            `json:"outputByteLimit"`
		TerminalID      string                         `json:"terminalId"`
	}
	if e := json.Unmarshal(raw, &p); e != nil {
		r.reject(id, e.Error())
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.proc != r || p.SessionID != s.id || s.turn == "" {
		r.reject(id, "session or turn is no longer active")
		return true
	}
	queue := func(req *adapter.ApprovalRequest, action func() (any, error)) {
		if s.opts.Mode.ReadOnly() {
			s.denied = method
			r.reject(id, "read-only session forbids "+method)
			return
		}
		if req.Class != adapter.ClassPolicy && pathsWithinWorkspace(req.Paths, s.opts.Cwd) {
			result, err := action()
			if err != nil {
				r.reject(id, err.Error())
			} else {
				r.respond(id, result)
			}
			return
		}
		req.ID = "kp_" + uuid.NewString()
		req.Cwd = s.opts.Cwd
		req.Raw = raw
		s.approvals[req.ID] = permission{wireID: id, class: req.Class, action: action}
		s.q.put(adapter.Event{Kind: adapter.EventApproval, TurnID: s.turn, Approval: req, Raw: raw})
	}
	switch method {
	case "fs/read_text_file":
		path, e := resolvedPath(p.Path)
		if e != nil {
			r.reject(id, e.Error())
			return true
		}
		data, e := os.ReadFile(path)
		if e != nil {
			r.reject(id, e.Error())
			return true
		}
		lines := strings.Split(string(data), "\n")
		start := p.Line - 1
		if start < 0 {
			start = 0
		}
		if start > len(lines) {
			start = len(lines)
		}
		end := len(lines)
		if p.Limit > 0 && p.Limit < end-start {
			end = start + p.Limit
		}
		r.respond(id, map[string]string{"content": strings.Join(lines[start:end], "\n")})
	case "fs/write_text_file":
		path, e := resolvedPath(p.Path)
		if e != nil {
			r.reject(id, e.Error())
			return true
		}
		queue(&adapter.ApprovalRequest{Class: adapter.ClassFileChange, Paths: []string{path}, Reason: "Kimi writes " + path}, func() (any, error) {
			// Recheck links at execution time so an intervening symlink swap cannot
			// turn an approved workspace path into an outside write.
			now, e := resolvedPath(path)
			if e != nil {
				return nil, e
			}
			if now != path {
				return nil, fmt.Errorf("write target changed since approval")
			}
			if e = os.MkdirAll(filepath.Dir(path), 0755); e != nil {
				return nil, e
			}
			return map[string]any{}, os.WriteFile(path, []byte(p.Content), 0644)
		})
	case "terminal/create":
		if p.Command == "" {
			r.reject(id, "empty terminal command")
			return true
		}
		cwd := p.Cwd
		if cwd == "" {
			cwd = s.opts.Cwd
		}
		cwd, e := resolvedPath(cwd)
		if e != nil {
			r.reject(id, e.Error())
			return true
		}
		command := p.Command
		for _, arg := range p.Args {
			command += " " + arg
		}
		// Kimi wraps Bash in a shell. Classify the actual script, so a leading
		// sudo/su is not hidden behind /bin/bash for the hub's privilege gate.
		base := filepath.Base(p.Command)
		if base == "bash" || base == "sh" || base == "zsh" {
			for i, arg := range p.Args {
				if strings.HasPrefix(arg, "-") && strings.Contains(arg, "c") && i+1 < len(p.Args) {
					command = p.Args[i+1]
					break
				}
			}
		}
		req := &adapter.ApprovalRequest{Class: adapter.ClassCommand, Command: command, Paths: []string{cwd}, Reason: "Kimi terminal command"}
		// Custom environment can replace loaders and policy variables. Make this
		// human-gated instead of presenting only the benign-looking executable.
		for _, v := range p.Env {
			if !ordinaryTerminalEnv(v.Name, v.Value) {
				req.Class = adapter.ClassPolicy
				req.Reason = "Kimi terminal command with environment overrides"
			}
		}
		if privilegeCommand.MatchString(command) {
			req.Class = adapter.ClassPolicy
			req.Reason = "Kimi shell command contains privilege escalation"
		}
		queue(req, func() (any, error) {
			cmd := exec.Command(p.Command, p.Args...)
			cmd.Dir = cwd
			cmd.Env = a.environment(s.opts)
			for _, v := range p.Env {
				if v.Name == "AGENT_HUB_TOKEN" {
					return nil, fmt.Errorf("hub credentials cannot be injected into a peer terminal")
				}
				cmd.Env = append(cmd.Env, v.Name+"="+v.Value)
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			limit := p.OutputByteLimit
			if limit <= 0 || limit > 1<<20 {
				limit = 1 << 20
			}
			term := &terminal{cmd: cmd, done: make(chan struct{}), output: terminalOutput{limit: limit}}
			cmd.Stdout = &term.output
			cmd.Stderr = &term.output
			if e := cmd.Start(); e != nil {
				return nil, e
			}
			tid := uuid.NewString()
			if s.terminals == nil {
				s.terminals = map[string]*terminal{}
			}
			s.terminals[tid] = term
			go func() { _ = cmd.Wait(); term.exitCode = cmd.ProcessState.ExitCode(); close(term.done) }()
			return map[string]string{"terminalId": tid}, nil
		})
	case "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		term := s.terminals[p.TerminalID]
		if term == nil {
			r.reject(id, "unknown terminal")
			return true
		}
		switch method {
		case "terminal/output":
			var exitStatus any
			select {
			case <-term.done:
				exitStatus = map[string]int{"exitCode": term.exitCode}
			default:
			}
			term.output.mu.Lock()
			result := map[string]any{"output": string(term.output.data), "truncated": term.output.truncated}
			term.output.mu.Unlock()
			if exitStatus != nil {
				result["exitStatus"] = exitStatus
			}
			r.respond(id, result)
		case "terminal/wait_for_exit":
			go func() {
				select {
				case <-term.done:
					r.respond(id, map[string]int{"exitCode": term.exitCode})
				case <-r.done:
				}
			}()
		case "terminal/kill":
			term.kill()
			r.respond(id, map[string]any{})
		case "terminal/release":
			term.kill()
			delete(s.terminals, p.TerminalID)
			r.respond(id, map[string]any{})
		}
	default:
		r.reject(id, "unsupported ACP client method "+method)
	}
	return true
}

// Paths have already been resolved, including symlinks, by the caller.
func pathsWithinWorkspace(paths []string, cwd string) bool {
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil || len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return true
}
