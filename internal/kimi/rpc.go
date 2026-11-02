package kimi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var errClosed = errors.New("kimi: ACP connection closed")

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("kimi RPC %d: %s (%s)", e.Code, e.Message, e.Data)
}

type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// One reader preserves notification order. Request handlers may park approvals
// without blocking responses to unrelated RPCs on the same connection.
type rpc struct {
	stderr  *stderrTail
	cmd     *exec.Cmd
	in      io.WriteCloser
	mu      sync.Mutex
	writeMu sync.Mutex
	seq     atomic.Int64
	pending map[int64]chan frame
	done    chan struct{}
	reaped  chan struct{}
	once    sync.Once
	notify  func(*rpc, string, json.RawMessage)
	request func(*rpc, json.RawMessage, string, json.RawMessage)
}

func dial(bin, cwd string, args, env []string, notify func(*rpc, string, json.RawMessage), request func(*rpc, json.RawMessage, string, json.RawMessage)) (*rpc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	var stderr stderrTail
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	r := &rpc{cmd: cmd, stderr: &stderr, in: in, pending: map[int64]chan frame{}, done: make(chan struct{}), reaped: make(chan struct{}), notify: notify, request: request}
	go func() { r.read(out); r.stop() }()
	return r, nil
}
func (r *rpc) read(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 65536), 32<<20)
	for sc.Scan() {
		var f frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		if f.Method != "" {
			if len(f.ID) > 0 {
				go r.request(r, f.ID, f.Method, f.Params)
			} else {
				r.notify(r, f.Method, f.Params)
			}
			continue
		}
		var id int64
		if json.Unmarshal(f.ID, &id) != nil {
			continue
		}
		r.mu.Lock()
		ch := r.pending[id]
		delete(r.pending, id)
		r.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	}
}
func (r *rpc) write(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	select {
	case <-r.done:
		<-r.reaped
		return fmt.Errorf("%w: %s", errClosed, r.stderr.String())
	default:
	}
	_, e = r.in.Write(append(b, '\n'))
	return e
}

type pendingCall struct {
	r  *rpc
	id int64
	ch chan frame
}

func (r *rpc) begin(method string, params any) (*pendingCall, error) {
	id := r.seq.Add(1)
	ch := make(chan frame, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	if err := r.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return nil, err
	}
	return &pendingCall{r: r, id: id, ch: ch}, nil
}
func (p *pendingCall) wait(ctx context.Context, result any) error {
	defer func() { p.r.mu.Lock(); delete(p.r.pending, p.id); p.r.mu.Unlock() }()
	decode := func(f frame) error {
		if f.Error != nil {
			return f.Error
		}
		if result != nil {
			return json.Unmarshal(f.Result, result)
		}
		return nil
	}
	select {
	case f := <-p.ch:
		return decode(f)
	case <-ctx.Done():
		return ctx.Err()
	case <-p.r.done:
		select {
		case f := <-p.ch:
			return decode(f)
		default:
		}
		<-p.r.reaped
		return fmt.Errorf("%w: %s", errClosed, p.r.stderr.String())
	}
}
func (r *rpc) call(ctx context.Context, method string, params, result any) error {
	p, err := r.begin(method, params)
	if err != nil {
		return err
	}
	return p.wait(ctx, result)
}
func (r *rpc) notification(method string, params any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (r *rpc) respond(id json.RawMessage, result any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func (r *rpc) reject(id json.RawMessage, reason string) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{Code: -32601, Message: reason}})
}
func (r *rpc) stop() {
	r.once.Do(func() {
		close(r.done)
		_ = r.in.Close()
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
		go func() {
			defer close(r.reaped)
			wait := make(chan struct{})
			go func() { _ = r.cmd.Wait(); close(wait) }()
			select {
			case <-wait:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
				<-wait
			}
		}()
	})
}
func (r *rpc) close() { r.stop(); <-r.reaped }

// Bound diagnostic memory even when a provider writes noisy stderr indefinitely.
type stderrTail struct {
	mu   sync.Mutex
	data []byte
}

func (b *stderrTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	const limit = 8192
	if n >= limit {
		b.data = append(b.data[:0], p[n-limit:]...)
	} else {
		if len(b.data)+n > limit {
			b.data = append([]byte(nil), b.data[len(b.data)+n-limit:]...)
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
func (b *stderrTail) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }
