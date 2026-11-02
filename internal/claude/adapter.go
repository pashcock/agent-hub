package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

type Options struct {
	Bin              string
	MaskMCPServer    string
	MaxLiveProcesses int
	IdleKill         time.Duration
}

// pendingTurn is one minted id awaiting its result.
type pendingTurn struct {
	id          string
	interrupted bool
}

// session is one conversation. It owns an event channel that outlives any
// individual process, so eviction and resume are invisible to the hub.
type session struct {
	id   string
	opts adapter.StartOptions
	ch   chan adapter.Event

	mu       sync.Mutex
	closed   bool
	proc     *Process
	pending  []pendingTurn
	lastUsed time.Time
	gen      int

	// backlog carries events that must not be dropped, drained by one
	// goroutine so emit never blocks the process reader.
	backMu   sync.Mutex
	backCond *sync.Cond
	backlog  []adapter.Event
	backDone bool
}

func newSession(id string, o adapter.StartOptions) *session {
	s := &session{id: id, opts: o, ch: make(chan adapter.Event, 256), lastUsed: time.Now()}
	s.backCond = sync.NewCond(&s.backMu)
	go s.drain()
	return s
}

// droppableBacklog is how many droppable events may pile up before new
// ones are discarded. Deltas are redundant: the full text also arrives in
// the result.
const droppableBacklog = 256

// enqueue is the only way an event reaches the session channel.
//
// Nothing else may send on s.ch, because the drain goroutine closes it.
// A droppable event that bypassed the backlog to do its own non-blocking
// send would race that close and panic. Dropping therefore happens here,
// on the backlog, rather than on the channel.
func (s *session) enqueue(ev adapter.Event, droppable bool) {
	s.backMu.Lock()
	defer s.backMu.Unlock()
	if s.backDone {
		return
	}
	if droppable && len(s.backlog) >= droppableBacklog {
		return
	}
	s.backlog = append(s.backlog, ev)
	s.backCond.Signal()
}

func (s *session) drain() {
	for {
		s.backMu.Lock()
		for len(s.backlog) == 0 && !s.backDone {
			s.backCond.Wait()
		}
		if s.backDone && len(s.backlog) == 0 {
			s.backMu.Unlock()
			close(s.ch)
			return
		}
		ev := s.backlog[0]
		s.backlog = s.backlog[1:]
		s.backMu.Unlock()
		s.ch <- ev
	}
}

func (s *session) shut() {
	s.backMu.Lock()
	s.backDone = true
	s.backCond.Signal()
	s.backMu.Unlock()
}

type Adapter struct {
	opts Options

	mu       sync.RWMutex
	sessions map[string]*session
	// capProbe serialises capability probes to bound process overhead.
	capProbe sync.Mutex
	// probes counts capability probes, which hold a process slot while
	// they run.
	probes int

	done      chan struct{}
	closeOnce sync.Once
}

func NewAdapter(o Options) *Adapter {
	if o.Bin == "" {
		o.Bin = "claude"
	}
	if o.MaxLiveProcesses < 1 {
		o.MaxLiveProcesses = 2
	}
	if o.IdleKill <= 0 {
		o.IdleKill = 15 * time.Minute
	}
	a := &Adapter{
		opts:     o,
		sessions: map[string]*session{},
		done:     make(chan struct{}),
	}
	go a.sweepIdle()
	return a
}

func (a *Adapter) Name() string { return "claude" }

// Profile: measured, not assumed. Claude queues input rather than
// absorbing it, has no filesystem sandbox, and raises no permission
// request to its host in -p mode.
func (a *Adapter) Profile() adapter.Profile {
	return adapter.Profile{Steer: false, SandboxedReadOnly: false, Approvals: false}
}

// spawnOptionsFor derives the child's flags.
//
// Both modes run the auto classifier. Measured on claude 2.1.267: auto
// is honoured in -p (in 2.0.x it was silently downgraded to default,
// which is why this used to say acceptEdits), it approves an MCP call
// with no grant, and it still denies a write when the writing tools are
// denied.
//
// Read-only is a tool denial, not plan mode. Plan mode refuses every MCP
// call outright — "Cannot call mcp__semble__search while in plan mode" —
// which took away exactly the tools a project configures for research:
// semble, the LSP servers, the IDE bridge. A denial of the writing tools
// keeps those working. Measured against a control run: Write is refused,
// while a shell redirect inside the working directory still writes, so
// this is a peer that does not edit files rather than a sealed sandbox.
// Codex is the peer with a real read-only sandbox.
func spawnOptionsFor(o Options, s adapter.StartOptions) SpawnOptions {
	sp := SpawnOptions{
		Bin: o.Bin, Cwd: s.Cwd, Model: s.Model, Effort: s.Effort,
		MaskMCPServer:  o.MaskMCPServer,
		PermissionMode: "auto",
	}
	if s.Mode.ReadOnly() {
		sp.DisallowedTools = []string{"Edit", "Write", "NotebookEdit"}
	}
	return sp
}

func (a *Adapter) StartSession(ctx context.Context, o adapter.StartOptions) (string, error) {
	// The hub names the session: system/init does not arrive until the
	// first message, so there is nothing to learn an id from yet.
	sid := NewSessionID()
	sp := spawnOptionsFor(a.opts, o)
	sp.SessionID = sid

	p, err := Spawn(ctx, sp)
	if err != nil {
		return "", err
	}
	s := newSession(sid, o)
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		p.Close()
		s.shut()
		return "", adapter.ErrSessionGone
	default:
	}
	a.sessions[sid] = s
	a.mu.Unlock()
	if err := a.attach(s, p); err != nil {
		s.shut()
		return "", err
	}
	a.evictIfNeeded(sid)
	return sid, nil
}

func (a *Adapter) ResumeSession(ctx context.Context, sessionID string, o adapter.StartOptions) error {
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return adapter.ErrSessionGone
	default:
	}
	s, ok := a.sessions[sessionID]
	if !ok {
		s = newSession(sessionID, o)
		a.sessions[sessionID] = s
	}
	a.mu.Unlock()

	s.mu.Lock()
	live := s.proc != nil
	s.mu.Unlock()
	if live {
		return nil
	}

	sp := spawnOptionsFor(a.opts, o)
	sp.ResumeSessionID = sessionID
	p, err := Spawn(ctx, sp)
	if err != nil {
		return err
	}
	if err := a.attach(s, p); err != nil {
		return err
	}
	a.evictIfNeeded(sessionID)
	return nil
}

func (a *Adapter) attach(s *session, p *Process) error {
	a.mu.RLock()
	s.mu.Lock()
	select {
	case <-a.done:
		s.closed = true
	default:
	}
	if s.closed || a.sessions[s.id] != s {
		s.mu.Unlock()
		a.mu.RUnlock()
		p.Close()
		return adapter.ErrSessionGone
	}
	if s.proc != nil {
		s.mu.Unlock()
		a.mu.RUnlock()
		p.Close()
		return nil
	}
	s.proc = p
	s.gen++
	gen := s.gen
	s.lastUsed = time.Now()
	s.mu.Unlock()
	a.mu.RUnlock()
	go a.pump(s, p, gen)
	return nil
}

// pump forwards one process's output. Every message is checked against
// the generation it belongs to, so output buffered from a process that
// has been replaced can never pop ids belonging to its successor.
func (a *Adapter) pump(s *session, p *Process, gen int) {
	for m := range p.Messages() {
		if !s.isCurrent(gen) {
			continue
		}
		a.translate(s, m)
	}
	a.onProcessGone(s, gen)
}

func (s *session) isCurrent(gen int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen == gen
}

// onProcessGone fails any turn still open. A released session has no
// pending turns, so a clean eviction produces no failure.
func (a *Adapter) onProcessGone(s *session, gen int) {
	s.mu.Lock()
	if s.gen != gen {
		s.mu.Unlock()
		return
	}
	s.proc = nil
	open := s.pending
	s.pending = nil
	s.mu.Unlock()

	for _, t := range open {
		s.enqueue(adapter.Event{
			Kind: adapter.EventTurnFailed, TurnID: t.id,
			Err: "claude process exited", At: time.Now()}, false)
	}
}

func (a *Adapter) translate(s *session, m Message) {
	switch m.Type {
	case MsgAssistant:
		if m.Activity != "" {
			a.emit(s, adapter.Event{
				Kind: adapter.EventActivity, TurnID: s.currentTurn(),
				Activity: m.Activity, Raw: m.Raw, At: time.Now()})
		}
		if m.Text != "" {
			a.emit(s, adapter.Event{
				Kind: adapter.EventMessageDelta, TurnID: s.currentTurn(),
				Text: m.Text, Raw: m.Raw, At: time.Now()})
		}
	case MsgResult:
		t, ok := s.popTurn()
		kind := adapter.EventTurnCompleted
		errText := ""
		switch {
		case t.interrupted || m.Subtype == SubtypeInterrupted:
			kind = adapter.EventInterrupted
		case m.IsError:
			kind = adapter.EventTurnFailed
			errText = m.Result
		}
		if t.interrupted {
			// The interruption was already reported when it was
			// requested; this late result only closes the bookkeeping.
			if !ok {
				return
			}
			s.touch()
			return
		}
		s.touch()
		a.emit(s, adapter.Event{
			Kind: kind, TurnID: t.id, Text: m.Result,
			Err: errText, Usage: usageOf(m), Raw: m.Raw, At: time.Now()})
	default:
		a.emit(s, adapter.Event{
			Kind: adapter.EventProviderRaw, TurnID: s.currentTurn(),
			Raw: m.Raw, At: time.Now()})
	}
}

// touch marks the session recently used, so a long turn is not evicted
// the instant it finishes.
func (s *session) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *session) currentTurn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return ""
	}
	return s.pending[0].id
}

func (s *session) popTurn() (pendingTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return pendingTurn{}, false
	}
	t := s.pending[0]
	s.pending = s.pending[1:]
	return t, true
}

// emit hands every event to the backlog, which is the only writer of the
// session channel. Deltas and raw provider lines are droppable once the
// backlog is deep; terminal events never are, because losing one would
// strand a turn.
func (a *Adapter) emit(s *session, ev adapter.Event) {
	switch ev.Kind {
	case adapter.EventMessageDelta, adapter.EventProviderRaw:
		s.enqueue(ev, true)
	default:
		s.enqueue(ev, false)
	}
}

func (a *Adapter) get(sessionID string) (*session, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.sessions[sessionID]
	if !ok {
		return nil, adapter.ErrSessionGone
	}
	return s, nil
}

func (a *Adapter) SendTurn(ctx context.Context, sessionID, text string, o adapter.TurnOptions) (string, error) {
	s, err := a.get(sessionID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p == nil {
		if err := a.ResumeSession(ctx, sessionID, s.opts); err != nil {
			return "", err
		}
		s.mu.Lock()
		p = s.proc
		s.mu.Unlock()
		if p == nil {
			return "", adapter.ErrSessionGone
		}
	}

	// Register the turn before sending. A fast result that arrived first
	// would otherwise take the wrong id and shift every later match.
	turnID := "ct_" + newRequestID()
	s.mu.Lock()
	s.pending = append(s.pending, pendingTurn{id: turnID})
	s.lastUsed = time.Now()
	s.mu.Unlock()

	if err := p.Send(text); err != nil {
		s.dropTurn(turnID)
		return "", err
	}
	a.emit(s, adapter.Event{Kind: adapter.EventTurnStarted, TurnID: turnID, At: time.Now()})
	return turnID, nil
}

// dropTurn removes a turn that never reached the provider.
func (s *session) dropTurn(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.pending {
		if t.id == turnID {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

// Steer is unsupported. Claude accepts a message during a turn, but it
// becomes a separate turn with its own result — one the hub has no row
// for, whose answer would be dropped and whose overlap would confuse the
// next peer_send. Callers poll and use peer_send instead.
func (a *Adapter) Steer(context.Context, string, string, string) error {
	return adapter.ErrUnsupported
}

func (a *Adapter) Interrupt(ctx context.Context, sessionID, turnID string) error {
	s, err := a.get(sessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	p := s.proc
	// Mark rather than pop: the provider still emits a result for an
	// interrupted turn, and that result is what closes the bookkeeping.
	for i := range s.pending {
		if s.pending[i].id == turnID {
			s.pending[i].interrupted = true
		}
	}
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	if err := p.Interrupt(ctx); err != nil {
		return err
	}
	a.emit(s, adapter.Event{Kind: adapter.EventInterrupted, TurnID: turnID, At: time.Now()})
	return nil
}

// Approve is unsupported by design. Measured: in -p mode Claude raises no
// permission request to its host, so there is nothing to answer.
func (a *Adapter) Approve(context.Context, string, string, adapter.Decision) error {
	return adapter.ErrUnsupported
}

func (a *Adapter) Events(sessionID string) (<-chan adapter.Event, error) {
	s, err := a.get(sessionID)
	if err != nil {
		return nil, err
	}
	return s.ch, nil
}

// Capabilities queries the native SDK control protocol without sending a
// user message. Each probe gets a fresh catalog so client updates and project
// model restrictions are reflected without restarting the hub.
func (a *Adapter) Capabilities(ctx context.Context, cwd string) (adapter.Capabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	a.capProbe.Lock()
	defer a.capProbe.Unlock()
	if err := ctx.Err(); err != nil {
		return adapter.Capabilities{}, err
	}

	a.mu.Lock()
	a.probes++
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.probes--
		a.mu.Unlock()
	}()
	a.evictIfNeeded("")

	sp := spawnOptionsFor(a.opts, adapter.StartOptions{Cwd: cwd, Mode: adapter.ModeAsk})
	sp.SessionID = NewSessionID()
	p, err := Spawn(ctx, sp)
	if err != nil {
		return adapter.Capabilities{}, err
	}
	defer p.Close()
	raw, err := p.control(ctx, "initialize")
	if err != nil {
		return adapter.Capabilities{}, err
	}
	models, err := parseModels(raw)
	if err != nil {
		return adapter.Capabilities{}, err
	}
	caps := adapter.Capabilities{
		Steer:             false,
		SandboxedReadOnly: false,
		Models:            models,
		MCPConnected:      map[string]bool{},
	}
	raw, err = p.control(ctx, "mcp_status")
	if err != nil {
		return adapter.Capabilities{}, err
	}
	var status struct {
		Servers []MCPServerStatus `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return adapter.Capabilities{}, fmt.Errorf("claude: mcp_status: %w", err)
	}
	if status.Servers == nil {
		return adapter.Capabilities{}, fmt.Errorf("claude: mcp_status missing mcpServers (unsupported protocol)")
	}
	for _, srv := range status.Servers {
		caps.MCPServers = append(caps.MCPServers, srv.Name)
		caps.MCPConnected[srv.Name] = srv.Status == "connected"
	}
	return caps, nil
}

func (a *Adapter) CloseSession(_ context.Context, sessionID string) error {
	a.mu.Lock()
	s, ok := a.sessions[sessionID]
	if !ok {
		a.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.closed = true
	delete(a.sessions, sessionID)
	p := s.proc
	s.proc = nil
	s.gen++
	s.mu.Unlock()
	a.mu.Unlock()
	if p != nil {
		p.Close()
	}
	s.shut()
	return nil
}

func (a *Adapter) Close() error {
	a.closeOnce.Do(func() { close(a.done) })
	a.mu.Lock()
	sessions := make([]*session, 0, len(a.sessions))
	for id, s := range a.sessions {
		sessions = append(sessions, s)
		delete(a.sessions, id)
	}
	a.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		s.closed = true
		p := s.proc
		s.proc = nil
		s.gen++
		s.mu.Unlock()
		if p != nil {
			p.Close()
		}
		s.shut()
	}
	return nil
}

// killForTest tears down a session's process without touching its state,
// so a test can observe an unexpected peer death.
func (a *Adapter) killForTest(sessionID string) {
	s, err := a.get(sessionID)
	if err != nil {
		return
	}
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p != nil {
		p.Close()
	}
}

func (a *Adapter) logEviction(sessionID, reason string) {
	slog.Info("claude session released", "session", sessionID, "reason", reason)
}

var _ adapter.Adapter = (*Adapter)(nil)

// usageOf turns a result line's measurements into the normalized form.
func usageOf(m Message) *adapter.Usage {
	if m.Usage == nil && m.CostUSD == 0 {
		return nil
	}
	u := &adapter.Usage{Model: m.Model, CostUSD: m.CostUSD, DurationMS: m.DurationMS}
	if m.Usage != nil {
		u.InputTokens = m.Usage.InputTokens
		u.OutputTokens = m.Usage.OutputTokens
		u.CachedInput = m.Usage.CacheReadInputTokens
		u.CacheWrite = m.Usage.CacheCreationInputTokens
		u.ReasoningTokens = m.Usage.OutputTokensDetails.ThinkingTokens
		u.TotalTokens = u.InputTokens + u.OutputTokens + u.CachedInput + u.CacheWrite
	}
	return u
}
