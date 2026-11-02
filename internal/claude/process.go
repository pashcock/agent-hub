package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
)

var ErrClosed = errors.New("claude: process closed")

type SpawnOptions struct {
	Bin             string
	Cwd             string
	SessionID       string
	ResumeSessionID string
	Model           string
	Effort          string
	PermissionMode  string
	AllowedTools    []string
	DisallowedTools []string
	Agent           string
	MaskMCPServer   string
	// Env overrides the child environment. Nil means "inherit, minus the
	// hub's own token".
	Env []string
}

// BuildArgs assembles the argv. Exported so the flags can be tested
// without spawning anything.
//
// --strict-mcp-config is deliberately absent: it would discard the
// project's own MCP servers, which is the native behaviour the hub exists
// to preserve. The hub's own server is denied by name instead.
func BuildArgs(o SpawnOptions) []string {
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose"}
	switch {
	case o.ResumeSessionID != "":
		args = append(args, "--resume", o.ResumeSessionID)
	case o.SessionID != "":
		args = append(args, "--session-id", o.SessionID)
	}
	for _, kv := range [][2]string{
		{"--model", o.Model}, {"--effort", o.Effort},
		{"--agent", o.Agent}, {"--permission-mode", o.PermissionMode},
	} {
		if kv[1] != "" {
			args = append(args, kv[0], kv[1])
		}
	}
	if len(o.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(o.AllowedTools, ","))
	}
	denied := slices.Clone(o.DisallowedTools)
	if o.MaskMCPServer != "" {
		denied = append(denied, "mcp__"+o.MaskMCPServer)
	}
	if len(denied) > 0 {
		args = append(args, "--disallowedTools", strings.Join(denied, ","))
	}
	return args
}

// Process is one supervised child, bound to one conversation for life.
type Process struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	msgs chan Message

	mu   sync.RWMutex
	init *InitInfo

	ctrl   sync.Mutex
	awaits map[string]chan Message

	errMu  sync.Mutex
	errBuf []byte

	stopOnce sync.Once
	stopping chan struct{} // closed to ask the reader to finish
	done     chan struct{} // closed by the reader once it has finished
}

func Spawn(ctx context.Context, o SpawnOptions) (*Process, error) {
	if o.Bin == "" {
		o.Bin = "claude"
	}
	cmd := exec.Command(o.Bin, BuildArgs(o)...)
	cmd.Dir = o.Cwd
	if o.Env != nil {
		cmd.Env = o.Env
	} else {
		// Inherit: project MCP servers and hooks depend on the
		// environment. Remove only the hub's own token, so a peer that
		// still sees the agent-hub entry cannot authenticate to it.
		cmd.Env = filterEnv(os.Environ(), "AGENT_HUB_TOKEN")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Capture stderr. Without this the child's diagnostics go nowhere,
	// and a peer that refuses to start looks identical to one that is
	// merely slow.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", o.Bin, err)
	}

	p := &Process{
		cmd: cmd, in: stdin,
		msgs:     make(chan Message, 256),
		awaits:   map[string]chan Message{},
		stopping: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go p.readLoop(stdout)
	go p.drainStderr(stderr)
	return p, nil
}

// stderrTail is how much of the child's stderr is kept for diagnostics.
const stderrTail = 8 << 10

func (p *Process) drainStderr(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			p.errMu.Lock()
			p.errBuf = append(p.errBuf, buf[:n]...)
			if len(p.errBuf) > stderrTail {
				p.errBuf = p.errBuf[len(p.errBuf)-stderrTail:]
			}
			p.errMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Stderr returns what the child last wrote to stderr, for error messages.
func (p *Process) Stderr() string {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return strings.TrimSpace(string(p.errBuf))
}

func filterEnv(environ []string, drop ...string) []string {
	out := environ[:0:0]
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(drop, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// readLoop owns p.msgs. It is the only goroutine that sends on it and the
// only one that closes it, so Close can never race a send.
func (p *Process) readLoop(stdout io.Reader) {
	defer func() {
		p.reap()
		close(p.msgs)
		close(p.done)
	}()
	sc := bufio.NewScanner(stdout)
	// A result carrying a long answer easily exceeds the default 64 KiB.
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	for sc.Scan() {
		select {
		case <-p.stopping:
			return
		default:
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		m := decode(line)
		if m.Type == "" {
			continue
		}
		if m.Init != nil {
			p.mu.Lock()
			p.init = m.Init
			p.mu.Unlock()
		}
		if m.Type == MsgControlResponse && m.RequestID != "" {
			p.ctrl.Lock()
			ch, ok := p.awaits[m.RequestID]
			delete(p.awaits, m.RequestID)
			p.ctrl.Unlock()
			if ok {
				ch <- m
				continue
			}
		}
		select {
		case p.msgs <- m:
		case <-p.stopping:
			return
		}
	}
}

func (p *Process) reap() {
	p.in.Close()
	if p.cmd.Process != nil {
		// Negative pid signals the whole process group.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	}
	_ = p.cmd.Wait()
}

func (p *Process) Init() *InitInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.init
}

func (p *Process) Messages() <-chan Message { return p.msgs }

// Done is closed once the reader has finished and p.msgs is closed.
func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) write(v any) error {
	b, err := jsonMarshalLine(v)
	if err != nil {
		return err
	}
	select {
	case <-p.stopping:
		return ErrClosed
	default:
	}
	_, err = p.in.Write(b)
	return err
}

// Send queues one user message. Claude answers one at a time and queues
// the rest.
func (p *Process) Send(text string) error { return p.write(userLine(text)) }

// Interrupt stops the running turn and waits for the acknowledgement. The
// provider still emits a result for the interrupted turn afterwards.
func (p *Process) Interrupt(ctx context.Context) error {
	_, err := p.control(ctx, "interrupt")
	return err
}

// control sends a native SDK control request without creating a model turn.
func (p *Process) control(ctx context.Context, subtype string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := newRequestID()
	ch := make(chan Message, 1)
	p.ctrl.Lock()
	p.awaits[id] = ch
	p.ctrl.Unlock()
	forget := func() {
		p.ctrl.Lock()
		delete(p.awaits, id)
		p.ctrl.Unlock()
	}

	defer forget()
	if err := p.write(map[string]any{
		"type": "control_request", "request_id": id,
		"request": map[string]any{"subtype": subtype},
	}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		var envelope struct {
			Response struct {
				Response json.RawMessage `json:"response"`
				Error    string          `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal(m.Raw, &envelope); err != nil {
			return nil, fmt.Errorf("claude: %s response: %w", subtype, err)
		}
		if m.Subtype != "success" {
			return nil, fmt.Errorf("claude: %s returned %q: %s", subtype, m.Subtype, envelope.Response.Error)
		}
		return envelope.Response.Response, nil
	case <-p.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close asks the reader to stop and waits for it. The reader closes the
// message channel, so no send can race the close.
func (p *Process) Close() error {
	p.stopOnce.Do(func() {
		close(p.stopping)
		// Closing stdin makes the child exit, which ends the read loop.
		p.in.Close()
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
		}
	})
	<-p.done
	return nil
}
