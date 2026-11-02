// Package codex drives a native Codex agent through `codex app-server`.
package codex

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
	"sync/atomic"
	"syscall"
)

// ErrClosed is returned to callers waiting on a connection that shut down.
var ErrClosed = errors.New("codex: connection closed")

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *wireError) Error() string {
	return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message)
}

type pending struct {
	result chan *wireMessage
}

// Conn is a line-framed JSON-RPC 2.0 connection to a child process.
// It is safe for concurrent use.
type Conn struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Scanner
	wmu  sync.Mutex
	seq  atomic.Int64
	pmu  sync.Mutex
	pend map[int64]*pending

	hmu       sync.RWMutex
	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage)

	closeOnce sync.Once
	closed    chan struct{}
}

// filterEnv returns environ without the named variables.
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

// Dial starts bin in dir and speaks JSON-RPC to it over stdio. The child
// runs in its own process group so Close can kill the whole tree.
//
// The child inherits the environment, because project MCP servers depend
// on it, minus the hub's own bearer token. That removal is the second half
// of the peer-masking guarantee: a peer that somehow still sees the
// agent-hub entry cannot authenticate to it.
func Dial(ctx context.Context, bin string, args []string, dir string) (*Conn, error) {
	return dial(ctx, bin, args, dir, filterEnv(os.Environ(), "AGENT_HUB_TOKEN"))
}

// dial is Dial with an explicit environment. Tests use it to build a
// reference process that still holds the token, so each half of the
// masking guarantee can be proved on its own.
func dial(ctx context.Context, bin string, args []string, dir string, env []string) (*Conn, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	sc := bufio.NewScanner(stdout)
	// app-server emits large turn/completed payloads; the default 64 KiB
	// token limit truncates them and corrupts the stream.
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)

	c := &Conn{
		cmd:    cmd,
		in:     stdin,
		out:    sc,
		pend:   map[int64]*pending{},
		closed: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Conn) OnNotification(fn func(method string, params json.RawMessage)) {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	c.onNotify = fn
}

func (c *Conn) OnServerRequest(fn func(id json.RawMessage, method string, params json.RawMessage)) {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	c.onRequest = fn
}

func (c *Conn) readLoop() {
	defer c.shutdown()
	for c.out.Scan() {
		line := c.out.Bytes()
		if len(line) == 0 {
			continue
		}
		var m wireMessage
		if err := json.Unmarshal(line, &m); err != nil {
			// app-server also writes human-readable warnings to stdout.
			// A line that is not JSON is not fatal.
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			c.hmu.RLock()
			fn := c.onRequest
			c.hmu.RUnlock()
			if fn != nil {
				id := append(json.RawMessage(nil), m.ID...)
				params := append(json.RawMessage(nil), m.Params...)
				go fn(id, m.Method, params)
			}
		case m.Method != "":
			c.hmu.RLock()
			fn := c.onNotify
			c.hmu.RUnlock()
			if fn != nil {
				params := append(json.RawMessage(nil), m.Params...)
				// Dispatched synchronously, on purpose. Provider event
				// order is meaningful: turn/started, then deltas, then
				// turn/completed. Handing each notification to its own
				// goroutine loses that order, and a consumer can then see
				// a turn complete before it ever saw it start.
				//
				// The contract for a notification handler is therefore
				// that it must not block. The adapter's handler only ever
				// does a non-blocking channel send or an append under a
				// mutex, so readLoop is never held up.
				fn(m.Method, params)
			}
		case len(m.ID) > 0:
			var id int64
			if err := json.Unmarshal(m.ID, &id); err != nil {
				continue
			}
			c.pmu.Lock()
			p, ok := c.pend[id]
			delete(c.pend, id)
			c.pmu.Unlock()
			if ok {
				cp := m
				p.result <- &cp
			}
		}
	}
}

func (c *Conn) writeMessage(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.in.Write(b)
	return err
}

// Call sends a request and waits for its response. result may be nil.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	id := c.seq.Add(1)
	p := &pending{result: make(chan *wireMessage, 1)}
	c.pmu.Lock()
	c.pend[id] = p
	c.pmu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := c.writeMessage(req); err != nil {
		c.forget(id)
		return fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("call %s: %w", method, ctx.Err())
	case <-c.closed:
		c.forget(id)
		return fmt.Errorf("call %s: %w", method, ErrClosed)
	case m := <-p.result:
		if m.Error != nil {
			return fmt.Errorf("call %s: %w", method, m.Error)
		}
		if result == nil || len(m.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(m.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	}
}

func (c *Conn) forget(id int64) {
	c.pmu.Lock()
	delete(c.pend, id)
	c.pmu.Unlock()
}

// Notify sends a request with no id and does not wait.
func (c *Conn) Notify(_ context.Context, method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	return c.writeMessage(req)
}

// Respond answers a server-to-client request.
func (c *Conn) Respond(id json.RawMessage, result any) error {
	if result == nil {
		result = map[string]any{}
	}
	return c.writeMessage(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// Done is closed when the connection dies for any reason: the child
// exited, its stdout closed, or Close was called. The adapter watches it
// so a provider crash fails every open turn instead of hanging it.
func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) shutdown() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.in.Close()
		if c.cmd.Process != nil {
			// Negative pid signals the whole process group.
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		}
		_ = c.cmd.Wait()
	})
}

func (c *Conn) Close() error {
	c.shutdown()
	return nil
}
