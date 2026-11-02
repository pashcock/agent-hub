// Package grok drives the native Grok Build harness over persistent ACP stdio.
package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pashcock/agent-hub/internal/adapter"
)

type Options struct {
	Bin, Home, AuthPath string
	MaxLiveProcesses    int
	IdleKill            time.Duration
}
type Adapter struct {
	opts          Options
	controlMu     sync.Mutex
	controlCtx    context.Context
	controlCancel context.CancelFunc
	lastAuth      time.Time
	mu            sync.Mutex
	sessions      map[string]*session
	closed        bool
	starting      int
	done          chan struct{}
	closeOnce     sync.Once
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
	approvals map[string]permission
	closed    bool
}
type permission struct {
	wireID  json.RawMessage
	options []permissionOption
	class   adapter.ApprovalClass
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
	a.controlCtx, a.controlCancel = context.WithCancel(context.Background())
	go a.sweepIdle()
	go a.maintainAuth()
	return a
}
func (a *Adapter) Name() string             { return "grok" }
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
		return fmt.Errorf("grok: process capacity reached; close an idle conversation")
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
func (a *Adapter) prepare() error {
	if a.opts.Home == "" || !filepath.IsAbs(a.opts.Home) {
		return fmt.Errorf("grok: an absolute private runtime home is required")
	}
	if err := os.MkdirAll(a.opts.Home, 0700); err != nil {
		return err
	}
	// This directory belongs to the hub, never to the user's interactive Grok.
	// The import marker prevents legacy settings from being merged at runtime.
	const config = `[claude_compat]
imported = true
[plugins]
disabled = ["codex", "superpowers"]
[ui]
permission_mode = "auto"
[cli]
auto_update = false
use_leader = false
`
	p := filepath.Join(a.opts.Home, "config.toml")
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	_, e = f.WriteString(config)
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func (a *Adapter) environment(o adapter.StartOptions) []string {
	overrides := map[string]string{"GROK_HOME": a.opts.Home, "GROK_AUTH_PATH": a.opts.AuthPath, "GROK_SANDBOX": "workspace", "GROK_SANDBOX_AUTO_ALLOW_BASH": "false"}
	if o.Mode.ReadOnly() {
		overrides["GROK_SANDBOX"] = "read-only"
	}
	for _, v := range []string{"CLAUDE", "CURSOR", "CODEX"} {
		for _, s := range []string{"SKILLS", "RULES", "AGENTS", "MCPS", "HOOKS", "SESSIONS"} {
			overrides["GROK_"+v+"_"+s+"_ENABLED"] = "false"
		}
	}
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[k]; ok {
			continue
		}
		switch k {
		case "AGENT_HUB_TOKEN", "XAI_API_KEY", "GROK_API_KEY", "GROK_AUTH", "GROK_CONFIG", "GROK_CONFIG_PATH":
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}
func (a *Adapter) connect(ctx context.Context, s *session) (*rpc, error) {
	if err := a.prepare(); err != nil {
		return nil, err
	}
	r, err := dial(a.opts.Bin, s.opts.Cwd, []string{"agent", "--no-leader", "stdio"}, a.environment(s.opts), func(r *rpc, m string, p json.RawMessage) { a.notification(s, r, m, p) }, func(r *rpc, id json.RawMessage, m string, p json.RawMessage) { a.request(s, r, id, m, p) })
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		r.close()
		return nil, adapter.ErrSessionGone
	}
	s.proc = r
	s.mu.Unlock()
	fail := func(e error) (*rpc, error) { s.mu.Lock(); s.proc = nil; s.mu.Unlock(); r.close(); return nil, e }
	var init struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
		AuthMethods []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
	}
	if err = r.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientInfo": map[string]string{"name": "agent-hub", "version": "0.1.0"}, "clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, &init); err != nil {
		return fail(err)
	}
	// Explicitly select cached subscription auth. Never silently switch to API billing.
	if err = r.call(ctx, "authenticate", map[string]any{"methodId": "cached_token", "_meta": map[string]any{"headless": true}}, nil); err != nil {
		return fail(fmt.Errorf("grok.com login required: %w", err))
	}
	if s.opts.Model != "" {
		if err := waitSessionModel(ctx, r, s.opts.Model); err != nil {
			return fail(err)
		}
	}
	return r, nil
}

// Each native process loads its own catalog asynchronously. A successful
// service probe does not mean this process has left its built-in fallback.
// Wait before creating/loading a session; never send a prompt on a substitute.
func waitSessionModel(ctx context.Context, r *rpc, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		var raw json.RawMessage
		if err := r.call(ctx, "_x.ai/models/list", map[string]any{}, &raw); err != nil {
			return fmt.Errorf("grok: waiting for requested model %q in session process: %w", model, err)
		}
		models, err := parseModels(raw)
		if err != nil {
			return fmt.Errorf("grok: session catalog: %w", err)
		}
		for _, m := range models {
			if m.ID == model {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("grok: requested model %q did not become available in session process: %w", model, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func validate(o adapter.StartOptions) error {
	if !filepath.IsAbs(o.Cwd) {
		return fmt.Errorf("grok: cwd must be absolute")
	}
	switch o.Mode {
	case adapter.ModeAsk, adapter.ModePlan, adapter.ModeReview, adapter.ModeCode:
	default:
		return fmt.Errorf("grok: invalid mode %q", o.Mode)
	}
	if o.Sandbox != "" {
		expected := "workspace-write"
		if o.Mode.ReadOnly() {
			expected = "read-only"
		}
		if o.Sandbox != expected {
			return fmt.Errorf("grok: unsupported sandbox %q for mode %q", o.Sandbox, o.Mode)
		}
	}
	switch o.Effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("grok: invalid effort %q", o.Effort)
	}
	if o.ServiceTier != "" || o.Personality != "" {
		return fmt.Errorf("grok: service tier and personality overrides are unsupported")
	}
	return nil
}
func (a *Adapter) StartSession(ctx context.Context, o adapter.StartOptions) (string, error) {
	if o.Model == "" {
		o.Model = "grok-4.7"
	}
	if err := validate(o); err != nil {
		return "", err
	}
	if err := a.reserve(); err != nil {
		return "", err
	}
	defer a.unreserve()
	s := &session{id: uuid.NewString(), opts: o, q: newQueue(), approvals: map[string]permission{}}
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
func (a *Adapter) attach(ctx context.Context, s *session, resume bool) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := a.controlCatalog(ctx, false); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return adapter.ErrSessionGone
	}
	s.lastUsed = time.Time{}
	s.mu.Unlock()
	r, err := a.connect(ctx, s)
	if err != nil {
		return err
	}
	meta := map[string]any{"autoMode": !s.opts.Mode.ReadOnly(), "yoloMode": false, "sessionKind": "headless"}
	if s.opts.Model != "" {
		meta["modelId"] = s.opts.Model
	}
	if s.opts.Effort != "" {
		meta["reasoningEffort"] = s.opts.Effort
	}
	params := map[string]any{"cwd": s.opts.Cwd, "mcpServers": []any{}, "_meta": meta}
	method := "session/new"
	if resume {
		method = "session/load"
		params["sessionId"] = s.id
	} else {
		meta["sessionId"] = s.id
	}
	var result struct {
		SessionID string `json:"sessionId"`
		Models    struct {
			Current   string `json:"currentModelId"`
			Available []struct {
				ID   string `json:"modelId"`
				Meta struct {
					Effort string `json:"reasoningEffort"`
				} `json:"_meta"`
			} `json:"availableModels"`
		} `json:"models"`
	}
	if err = r.call(ctx, method, params, &result); err == nil && !resume && result.SessionID != s.id {
		err = fmt.Errorf("grok: session id mismatch: requested %s, got %s", s.id, result.SessionID)
	}
	if err == nil && s.opts.Model != "" && result.Models.Current != s.opts.Model {
		err = fmt.Errorf("grok selected %q instead of requested model %q", result.Models.Current, s.opts.Model)
	}
	if err == nil && s.opts.Effort != "" {
		matched := false
		for _, m := range result.Models.Available {
			if m.ID == result.Models.Current && m.Meta.Effort == s.opts.Effort {
				matched = true
			}
		}
		if !matched {
			err = fmt.Errorf("grok did not confirm requested reasoning effort %q", s.opts.Effort)
		}
	}
	if err == nil {
		err = verifyIsolation(ctx, r, s.id)
	}
	if err != nil {
		s.mu.Lock()
		s.proc = nil
		s.mu.Unlock()
		r.close()
		return err
	}
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
	return nil
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
			return fmt.Errorf("grok: cannot change session launch options on resume")
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
		return "", fmt.Errorf("grok: model/effort changes require a new conversation")
	}
	if _, err := a.controlCatalog(ctx, false); err != nil {
		return "", err
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
		return "", fmt.Errorf("grok: session busy or closed")
	}
	turn := "gt_" + uuid.NewString()
	s.turn = turn
	s.text.Reset()
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
	s.approvals = map[string]permission{}
	ev := adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: turn, Text: s.text.String(), Raw: raw}
	if err != nil {
		ev.Kind = adapter.EventTurnFailed
		ev.Err = err.Error()
	} else {
		var v struct {
			StopReason string `json:"stopReason"`
			Meta       struct {
				Model string `json:"modelId"`
				Usage *usage `json:"usage"`
			} `json:"_meta"`
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
					ev.Err = "Grok read-only permission denied: " + s.denied
				}
			case "end_turn":
			default:
				ev.Kind = adapter.EventTurnFailed
				ev.Err = "Grok stopped without completing the turn: " + v.StopReason
			}
			if v.Meta.Usage != nil {
				ev.Usage = v.Meta.Usage.normalized(v.Meta.Model)
			}
		}
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
		s.q.put(adapter.Event{Kind: adapter.EventTurnFailed, TurnID: s.turn, Err: "Grok process exited. Changes may remain on disk; peer_send resumes the saved conversation."})
		s.turn = ""
	}
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
		return fmt.Errorf("grok: turn is no longer active")
	}
	for _, p := range s.approvals {
		_ = s.proc.respond(p.wireID, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
	}
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
	a.closeOnce.Do(func() { close(a.done); a.controlCancel() })
	// Reap any service probe before returning from shutdown.
	a.controlMu.Lock()
	a.controlMu.Unlock()
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
func (a *Adapter) Capabilities(ctx context.Context, cwd string) (adapter.Capabilities, error) {
	if err := a.reserve(); err != nil {
		return adapter.Capabilities{}, err
	}
	defer a.unreserve()
	// Discovery is account-level. Never execute a privileged service probe in
	// a caller's project; project policy still applies to sandboxed sessions.
	models, err := a.controlCatalog(ctx, true)
	return adapter.Capabilities{Models: models}, err
}

var _ adapter.Adapter = (*Adapter)(nil)
