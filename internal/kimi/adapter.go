// Package kimi drives native Kimi Code over persistent ACP stdio.
package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pashcock/agent-hub/internal/adapter"
)

type Options struct {
	Bin, Home, SourceHome string
	MaxLiveProcesses      int
	IdleKill              time.Duration
}
type Adapter struct {
	opts      Options
	prepareMu sync.Mutex
	mu        sync.Mutex
	sessions  map[string]*session
	closed    bool
	starting  int
	done      chan struct{}
	closeOnce sync.Once
}
type session struct {
	mu        sync.Mutex
	lifecycle sync.Mutex
	lastUsed  time.Time
	id        string
	opts      adapter.StartOptions
	proc      *rpc
	q         *eventQueue
	turn      string
	text      strings.Builder
	denied    string
	toolCalls map[string]permissionToolCall
	approvals map[string]permission
	terminals map[string]*terminal
	closed    bool
}
type permission struct {
	wireID  json.RawMessage
	options []permissionOption
	class   adapter.ApprovalClass
	action  func() (any, error)
}
type permissionOption struct {
	ID   string `json:"optionId"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func NewAdapter(o Options) *Adapter {
	if o.MaxLiveProcesses <= 0 {
		o.MaxLiveProcesses = 4
	}
	if o.IdleKill <= 0 {
		o.IdleKill = 15 * time.Minute
	}
	a := &Adapter{opts: o, sessions: map[string]*session{}, done: make(chan struct{})}
	go a.sweepIdle()
	return a
}
func (a *Adapter) Name() string             { return "kimi" }
func (a *Adapter) Profile() adapter.Profile { return adapter.Profile{Approvals: true} }
func (a *Adapter) Capacity() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.starting
	for _, s := range a.sessions {
		s.mu.Lock()
		if s.proc != nil {
			n++
		}
		s.mu.Unlock()
	}
	return n, a.opts.MaxLiveProcesses
}
func (a *Adapter) reserve() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errClosed
	}
	n := a.starting
	for _, s := range a.sessions {
		s.mu.Lock()
		if s.proc != nil {
			n++
		}
		s.mu.Unlock()
	}
	if n >= a.opts.MaxLiveProcesses {
		return fmt.Errorf("kimi: process capacity reached; close an idle conversation")
	}
	a.starting++
	return nil
}
func (a *Adapter) unreserve() { a.mu.Lock(); a.starting--; a.mu.Unlock() }
func (a *Adapter) get(id string) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[id]
	if s == nil {
		return nil, adapter.ErrSessionGone
	}
	return s, nil
}
func validate(o adapter.StartOptions) error {
	if !filepath.IsAbs(o.Cwd) {
		return fmt.Errorf("kimi: cwd must be absolute")
	}
	switch o.Mode {
	case adapter.ModeAsk, adapter.ModePlan, adapter.ModeReview, adapter.ModeCode:
	default:
		return fmt.Errorf("kimi: invalid mode %q", o.Mode)
	}
	if o.Sandbox != "" {
		return fmt.Errorf("kimi: native filesystem sandbox overrides are unsupported")
	}
	if o.ServiceTier != "" || o.Personality != "" {
		return fmt.Errorf("kimi: service tier and personality overrides are unsupported")
	}
	return nil
}
func (a *Adapter) StartSession(ctx context.Context, o adapter.StartOptions) (string, error) {
	if err := validate(o); err != nil {
		return "", err
	}
	if err := a.reserve(); err != nil {
		return "", err
	}
	defer a.unreserve()
	s := &session{opts: o, q: newQueue(), approvals: map[string]permission{}}
	if err := a.attach(ctx, s, false); err != nil {
		s.q.close()
		return "", err
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		s.proc.close()
		s.q.close()
		return "", errClosed
	}
	a.sessions[s.id] = s
	s.mu.Lock()
	r := s.proc
	s.mu.Unlock()
	a.mu.Unlock()
	go a.watch(s, r)
	return s.id, nil
}
func (a *Adapter) ResumeSession(ctx context.Context, id string, o adapter.StartOptions) error {
	if err := validate(o); err != nil {
		return err
	}
	s, _ := a.get(id)
	if s != nil {
		s.lifecycle.Lock()
		defer s.lifecycle.Unlock()
		s.mu.Lock()
		live := s.proc != nil
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return adapter.ErrSessionGone
		}
		if live {
			return nil
		}
		// An omitted sandbox means restore the pinned value, including
		// sessions started before the hub persisted this field.
		if o.Sandbox == "" {
			o.Sandbox = s.opts.Sandbox
		}
		if s.opts != o {
			return fmt.Errorf("kimi: cannot change session launch options on resume")
		}
	}
	if err := a.reserve(); err != nil {
		return err
	}
	defer a.unreserve()
	created := s == nil
	if created {
		s = &session{id: id, opts: o, q: newQueue(), approvals: map[string]permission{}}
	}
	if err := a.attach(ctx, s, true); err != nil {
		if created {
			s.q.close()
		}
		return err
	}
	a.mu.Lock()
	s.mu.Lock()
	if a.closed || s.closed {
		r := s.proc
		s.proc = nil
		s.mu.Unlock()
		a.mu.Unlock()
		if r != nil {
			r.close()
		}
		s.q.close()
		return adapter.ErrSessionGone
	}
	a.sessions[id] = s
	r := s.proc
	s.mu.Unlock()
	a.mu.Unlock()
	go a.watch(s, r)
	return nil
}
func (a *Adapter) SendTurn(ctx context.Context, id, text string, o adapter.TurnOptions) (string, error) {
	s, err := a.get(id)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	live := s.proc != nil
	s.mu.Unlock()
	if !live {
		if err = a.ResumeSession(ctx, id, s.opts); err != nil {
			return "", err
		}
	}
	// Model/effort stay pinned for the native session in this first adapter.
	if (o.Model != "" && o.Model != s.opts.Model) || (o.Effort != "" && o.Effort != s.opts.Effort) {
		return "", fmt.Errorf("kimi: model/effort changes require a new conversation")
	}
	for {
		s.mu.Lock()
		if s.proc != nil || s.closed {
			break
		}
		s.mu.Unlock()
		if err = a.ResumeSession(ctx, id, s.opts); err != nil {
			return "", err
		}
	}
	if s.turn != "" || s.closed {
		s.mu.Unlock()
		return "", fmt.Errorf("kimi: session busy or closed")
	}
	turn := "kt_" + uuid.NewString()
	s.turn = turn
	s.text.Reset()
	s.toolCalls = nil
	s.denied = ""
	r := s.proc
	s.mu.Unlock()

	pending, err := r.begin("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": text}}})
	if err != nil {
		s.mu.Lock()
		s.turn = ""
		s.mu.Unlock()
		return "", err
	}
	s.q.put(adapter.Event{Kind: adapter.EventTurnStarted, TurnID: turn})
	go func() {
		var result json.RawMessage
		err := pending.wait(context.Background(), &result)
		a.finish(s, r, turn, result, err)
	}()

	return turn, nil
}
func (a *Adapter) finish(s *session, r *rpc, turn string, raw json.RawMessage, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != turn || s.proc != r {
		return
	}
	s.turn = ""
	s.lastUsed = time.Now()
	s.killTerminals()
	s.approvals = map[string]permission{}
	ev := adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: turn, Text: s.text.String(), Raw: raw}
	if err != nil {
		ev.Kind = adapter.EventTurnFailed
		ev.Err = err.Error()
	} else {
		var v struct {
			StopReason string `json:"stopReason"`
		}
		if e := json.Unmarshal(raw, &v); e != nil {
			ev.Kind = adapter.EventTurnFailed
			ev.Err = e.Error()
		} else {
			switch v.StopReason {
			case "cancelled":
				ev.Kind = adapter.EventInterrupted
				if s.denied != "" {
					ev.Kind = adapter.EventTurnFailed
					ev.Err = "Kimi read-only permission denied: " + s.denied
				}
			case "end_turn":
			default:
				ev.Kind = adapter.EventTurnFailed
				ev.Err = "Kimi stopped without completing the turn: " + v.StopReason
			}
		}
	}
	if s.denied != "" {
		ev.Kind = adapter.EventTurnFailed
		ev.Err = "Kimi read-only permission denied: " + s.denied
	}
	s.q.put(ev)
}
func (a *Adapter) watch(s *session, r *rpc) {
	<-r.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != r || s.closed {
		return
	}
	s.proc = nil
	if s.turn != "" {
		s.q.put(adapter.Event{Kind: adapter.EventTurnFailed, TurnID: s.turn, Err: "Kimi process exited. Changes may remain on disk; peer_send resumes the saved conversation."})
		s.turn = ""
	}
	s.killTerminals()
	s.approvals = map[string]permission{}
	s.q.put(adapter.Event{Kind: adapter.EventSessionCold})
}
func (a *Adapter) Steer(context.Context, string, string, string) error { return adapter.ErrUnsupported }
func (a *Adapter) Interrupt(ctx context.Context, id, turn string) error {
	s, e := a.get(id)
	if e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return errClosed
	}
	if s.turn != turn {
		return fmt.Errorf("kimi: turn is no longer active")
	}
	for _, p := range s.approvals {
		if p.action != nil {
			_ = s.proc.reject(p.wireID, "turn cancelled")
		} else {
			_ = s.proc.respond(p.wireID, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
		}
	}
	s.killTerminals()
	s.approvals = map[string]permission{}
	return s.proc.notification("session/cancel", map[string]string{"sessionId": id})
}
func (a *Adapter) Events(id string) (<-chan adapter.Event, error) {
	s, e := a.get(id)
	if e != nil {
		return nil, e
	}
	return s.q.out, nil
}
func (a *Adapter) CloseSession(ctx context.Context, id string) error {
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil {
		a.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.killTerminals()
	delete(a.sessions, id)
	r := s.proc
	s.proc = nil
	s.mu.Unlock()
	a.mu.Unlock()
	if r != nil {
		r.close()
	}
	s.q.close()
	return nil
}
func (a *Adapter) Close() error {
	a.closeOnce.Do(func() { close(a.done) })
	a.mu.Lock()
	a.closed = true
	ids := []string{}
	for id := range a.sessions {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		_ = a.CloseSession(context.Background(), id)
	}
	return nil
}

var _ adapter.Adapter = (*Adapter)(nil)
