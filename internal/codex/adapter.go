package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

type AdapterOptions struct {
	DefaultCwd string
	IdleKill   time.Duration
}

// threadState owns one session's event stream. Droppable events go
// straight to ch; everything else queues in backlog so a slow or absent
// reader can never cause an event to be lost.
type threadState struct {
	ch chan adapter.Event

	mu      sync.Mutex
	cond    *sync.Cond
	backlog []adapter.Event
	closed  bool
	// openTurn is the turn running on this thread right now, if any.
	// The connection watchdog needs it by id: a turn whose process died
	// has to be failed explicitly, or the hub leaves it `running`
	// forever and never frees its concurrency slot.
	openTurn string
	lastUsed time.Time
	cold     bool
	options  adapter.StartOptions
}

func newThreadState() *threadState {
	st := &threadState{ch: make(chan adapter.Event, 256), lastUsed: time.Now()}
	st.cond = sync.NewCond(&st.mu)
	go st.drain()
	return st
}

// droppableBacklog is how many droppable events may pile up before new
// ones are discarded. Deltas are redundant: the full text also arrives in
// turn/completed.
const droppableBacklog = 256

// enqueue is the only way an event reaches the session channel.
//
// Nothing else may send on st.ch, because the drain goroutine closes it.
// A droppable event that bypassed the backlog to do its own non-blocking
// send would race that close and panic, which is exactly what happens
// once sessions are shut while deltas are still flowing.
func (st *threadState) enqueue(ev adapter.Event, droppable bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return
	}
	if droppable && len(st.backlog) >= droppableBacklog {
		return
	}
	st.backlog = append(st.backlog, ev)
	st.cond.Signal()
}

// markTurn records the turn that is now running on this thread.
func (st *threadState) markTurn(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.openTurn = id
	st.lastUsed = time.Now()
}

// takeTurn clears the open turn and reports what it was.
func (st *threadState) takeTurn() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	id := st.openTurn
	st.openTurn = ""
	st.lastUsed = time.Now()
	return id
}

func (st *threadState) drain() {
	for {
		st.mu.Lock()
		for len(st.backlog) == 0 && !st.closed {
			st.cond.Wait()
		}
		if st.closed && len(st.backlog) == 0 {
			st.mu.Unlock()
			close(st.ch)
			return
		}
		ev := st.backlog[0]
		st.backlog = st.backlog[1:]
		st.mu.Unlock()
		st.ch <- ev // Blocks until the hub's pump takes it. That is correct.
	}
}

// shut stops the drain goroutine after the backlog is delivered. It closes
// ch exactly once, from the drain goroutine, so no send can race a close.
func (st *threadState) shut() {
	st.mu.Lock()
	st.closed = true
	st.cond.Signal()
	st.mu.Unlock()
}

// Adapter drives Codex threads over one app-server connection. One
// process serves every conversation; a thread is created per conversation.
type Adapter struct {
	opts      AdapterOptions
	lifecycle sync.Mutex // Serializes subscription changes with turn submission.

	mu        sync.RWMutex
	conn      *Conn
	threads   map[string]*threadState
	approvals map[string]pendingApproval

	// dial makes a fresh app-server connection. The adapter owns one
	// child for the daemon's life, so when it dies the adapter has to
	// bring it back rather than staying dead until the hub restarts.
	dial      func(context.Context) (*Conn, error)
	done      chan struct{}
	closeOnce sync.Once
}

// reconnectBackoff is how long to wait between respawn attempts. A child
// that cannot start is usually a broken install or an expired login, so
// retrying fast would only spin.
var reconnectBackoff = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second,
}

func backoffFor(attempt int) time.Duration {
	if attempt >= len(reconnectBackoff) {
		return reconnectBackoff[len(reconnectBackoff)-1]
	}
	return reconnectBackoff[attempt]
}

// conn returns the live connection, or nil while the adapter is between
// children.
func (a *Adapter) current() *Conn {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.conn
}

// call runs one RPC against whatever connection is current.
func (a *Adapter) call(ctx context.Context, method string, params, result any) error {
	c := a.current()
	if c == nil {
		return fmt.Errorf("codex: app-server is not connected")
	}
	return c.Call(ctx, method, params, result)
}

// NewAdapter performs the initialize handshake and wires event routing.
// A dial function lets the adapter replace its child if that child dies;
// without one it works exactly as before but cannot recover.
func NewAdapter(ctx context.Context, conn *Conn, opts AdapterOptions) (*Adapter, error) {
	return newAdapter(ctx, conn, opts, nil)
}

func newAdapter(ctx context.Context, conn *Conn, opts AdapterOptions,
	dial func(context.Context) (*Conn, error)) (*Adapter, error) {
	if opts.IdleKill <= 0 {
		opts.IdleKill = 15 * time.Minute
	}
	a := &Adapter{
		opts:      opts,
		conn:      conn,
		threads:   map[string]*threadState{},
		approvals: map[string]pendingApproval{},
		dial:      dial,
		done:      make(chan struct{}),
	}
	if err := a.handshake(ctx, conn); err != nil {
		return nil, err
	}
	go a.watchConn()
	go a.sweepIdle()
	return a, nil
}

// handshake wires routing and completes initialize on one connection.
func (a *Adapter) handshake(ctx context.Context, conn *Conn) error {
	conn.OnNotification(a.handleNotification)
	conn.OnServerRequest(a.handleServerRequest)
	if err := conn.Call(ctx, MethodInitialize, InitializeParams{
		ClientInfo:   ClientInfo{Name: "agent-hub", Version: "0.1.0"},
		Capabilities: InitializeCapabilities{ExperimentalAPI: true},
	}, nil); err != nil {
		return fmt.Errorf("codex initialize: %w", err)
	}
	if err := conn.Notify(ctx, MethodInitialized, map[string]any{}); err != nil {
		return fmt.Errorf("codex initialized: %w", err)
	}
	return nil
}

func (a *Adapter) Name() string { return "codex" }

// Profile: Codex absorbs steering input mid-turn, enforces read-only in
// its own sandbox, and raises approval requests to its client.
func (a *Adapter) Profile() adapter.Profile {
	return adapter.Profile{Steer: true, SandboxedReadOnly: true, Approvals: true}
}

// Capacity: every thread lives in the one app-server process, so there is
// no per-session process to run out of.
func (a *Adapter) Capacity() (int, int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.threads), 0
}

// sandboxFor derives a Codex sandbox mode from the requested mode unless
// the caller pinned one explicitly.
func sandboxFor(opts adapter.StartOptions) string {
	if opts.Sandbox != "" {
		return opts.Sandbox
	}
	if opts.Mode.ReadOnly() {
		return "read-only"
	}
	return "workspace-write"
}

func (a *Adapter) cwd(opts adapter.StartOptions) string {
	if opts.Cwd != "" {
		return opts.Cwd
	}
	return a.opts.DefaultCwd
}

func (a *Adapter) StartSession(ctx context.Context, opts adapter.StartOptions) (string, error) {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	var res ThreadResult
	err := a.call(ctx, MethodThreadStart, ThreadStartParams{
		Cwd:            a.cwd(opts),
		Model:          opts.Model,
		Sandbox:        sandboxFor(opts),
		ApprovalPolicy: "on-request",
		ServiceTier:    opts.ServiceTier,
		Personality:    opts.Personality,
	}, &res)
	if err != nil {
		return "", err
	}
	if res.Thread.ID == "" {
		return "", fmt.Errorf("codex: thread/start returned no thread id")
	}
	a.registerOptions(res.Thread.ID, opts)
	return res.Thread.ID, nil
}

func (a *Adapter) ResumeSession(ctx context.Context, sessionID string, opts adapter.StartOptions) error {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	return a.resume(ctx, sessionID, opts)
}

func (a *Adapter) resume(ctx context.Context, sessionID string, opts adapter.StartOptions) error {
	err := a.call(ctx, MethodThreadResume, ThreadResumeParams{
		ThreadID:       sessionID,
		Cwd:            a.cwd(opts),
		Sandbox:        sandboxFor(opts),
		ApprovalPolicy: "on-request",
		Model:          opts.Model,
	}, nil)
	if err != nil {
		// app-server reports an unknown thread as a plain RPC error.
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return adapter.ErrSessionGone
		}
		return err
	}
	a.registerOptions(sessionID, opts)
	return nil
}

func (a *Adapter) register(threadID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.threads[threadID]; !ok {
		a.threads[threadID] = newThreadState()
	}
}

func (a *Adapter) SendTurn(ctx context.Context, sessionID, text string, opts adapter.TurnOptions) (string, error) {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	a.mu.RLock()
	st := a.threads[sessionID]
	a.mu.RUnlock()
	if st == nil {
		return "", adapter.ErrSessionGone
	}
	st.mu.Lock()
	cold, options := st.cold, st.options
	st.mu.Unlock()
	if cold {
		if err := a.resume(ctx, sessionID, options); err != nil {
			return "", err
		}
	}
	// Notifications can complete a turn before the RPC response arrives.
	st.markTurn("starting")
	var res TurnResult
	err := a.call(ctx, MethodTurnStart, TurnStartParams{
		ThreadID: sessionID, Input: TextInput(text), Model: opts.Model, Effort: opts.Effort,
	}, &res)
	st.mu.Lock()
	if st.openTurn == "starting" {
		st.openTurn = res.Turn.ID
		if err != nil {
			st.openTurn = ""
		}
	}
	st.lastUsed = time.Now()
	st.mu.Unlock()
	if err != nil {
		return "", err
	}
	return res.Turn.ID, nil
}

func (a *Adapter) Events(sessionID string) (<-chan adapter.Event, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	st, ok := a.threads[sessionID]
	if !ok {
		return nil, adapter.ErrSessionGone
	}
	return st.ch, nil
}

// emit hands every event to the backlog, which is the only writer of the
// session channel.
//
// Deltas are the only droppable kind: they are redundant because the full
// text also arrives in turn/completed. Everything else — turn_completed,
// turn_failed, turn_interrupted, approval_requested — changes hub state,
// so losing one strands a turn in `running` forever and leaks its
// concurrency slot.
func (a *Adapter) emit(threadID string, ev adapter.Event) {
	a.mu.RLock()
	st, ok := a.threads[threadID]
	a.mu.RUnlock()
	if !ok {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	switch ev.Kind {
	case adapter.EventTurnStarted:
		st.markTurn(ev.TurnID)
	case adapter.EventTurnCompleted, adapter.EventTurnFailed, adapter.EventInterrupted:
		st.takeTurn()
	}
	st.enqueue(ev, ev.Kind == adapter.EventMessageDelta)
}

func (a *Adapter) handleNotification(method string, params json.RawMessage) {
	switch method {
	case NotifyTurnStarted:
		var p TurnNotification
		if json.Unmarshal(params, &p) != nil {
			return
		}
		a.emit(p.ThreadID, adapter.Event{
			Kind: adapter.EventTurnStarted, TurnID: p.Turn.ID, Raw: params})

	case NotifyMessageDelta:
		var p DeltaNotification
		if json.Unmarshal(params, &p) != nil {
			return
		}
		a.emit(p.ThreadID, adapter.Event{
			Kind: adapter.EventMessageDelta, TurnID: p.TurnID, Text: p.Delta, Raw: params})

	case NotifyItemStarted:
		var p ItemNotification
		if json.Unmarshal(params, &p) != nil {
			return
		}
		if label := activityLabel(p.Item); label != "" {
			a.emit(p.ThreadID, adapter.Event{
				Kind: adapter.EventActivity, TurnID: p.TurnID,
				Activity: label, Raw: params})
		}

	case NotifyItemCompleted:
		var p ItemNotification
		if json.Unmarshal(params, &p) != nil {
			return
		}
		a.emit(p.ThreadID, adapter.Event{
			Kind: adapter.EventItemCompleted, TurnID: p.TurnID,
			Text: p.Item.Text, Raw: params})

	case NotifyTurnCompleted:
		var p TurnNotification
		if json.Unmarshal(params, &p) != nil {
			return
		}
		kind := adapter.EventTurnCompleted
		switch p.Turn.Status {
		case "interrupted":
			kind = adapter.EventInterrupted
		case "failed":
			kind = adapter.EventTurnFailed
		}
		a.emit(p.ThreadID, adapter.Event{
			Kind: kind, TurnID: p.Turn.ID, Text: finalText(p.Turn), Raw: params})

	case NotifyTokenUsage:
		var p TokenUsageNotification
		if json.Unmarshal(params, &p) != nil {
			return
		}
		b := p.TokenUsage.Last
		a.emit(p.ThreadID, adapter.Event{
			Kind: adapter.EventTokenUsage, TurnID: p.TurnID, Raw: params,
			Usage: &adapter.Usage{
				InputTokens: b.InputTokens, OutputTokens: b.OutputTokens,
				CachedInput: b.CachedInputTokens, CacheWrite: b.CacheWriteInputTokens,
				ReasoningTokens: b.ReasoningOutputTokens, TotalTokens: b.TotalTokens,
			}})

	case NotifyError:
		// app-server reports recoverable trouble here. It carries no
		// thread id, so it cannot fail a specific turn; record it on
		// every live session as a provider_raw event for the log.
		a.mu.RLock()
		ids := make([]string, 0, len(a.threads))
		for id := range a.threads {
			ids = append(ids, id)
		}
		a.mu.RUnlock()
		for _, id := range ids {
			a.emit(id, adapter.Event{Kind: adapter.EventProviderRaw, Raw: params})
		}
	}
}

// finalText concatenates the agentMessage items of a completed turn.
func finalText(t Turn) string {
	var b strings.Builder
	for _, it := range t.Items {
		if it.Type == "agentMessage" && it.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(it.Text)
		}
	}
	return b.String()
}

func (a *Adapter) Capabilities(ctx context.Context, _ string) (adapter.Capabilities, error) {
	caps := adapter.Capabilities{Steer: true, SandboxedReadOnly: true}

	params := map[string]any{"includeHidden": false}
	seenCursors := map[string]bool{}
	for {
		var models ModelListResult
		if err := a.call(ctx, MethodModelList, params, &models); err != nil {
			return caps, fmt.Errorf("model/list: %w", err)
		}
		for _, m := range models.Data {
			if m.Hidden {
				continue
			}
			id := m.Model
			if id == "" {
				id = m.ID
			}
			out := adapter.Model{ID: id, DefaultEffort: m.DefaultReasoningEffort}
			for _, e := range m.SupportedReasoningEfforts {
				out.Efforts = append(out.Efforts, e.ReasoningEffort)
			}
			caps.Models = append(caps.Models, out)
		}
		if models.NextCursor == "" {
			break
		}
		if seenCursors[models.NextCursor] {
			return caps, fmt.Errorf("model/list: repeated pagination cursor %q", models.NextCursor)
		}
		seenCursors[models.NextCursor] = true
		params["cursor"] = models.NextCursor
	}

	// Do not swallow this error. A masking test that asserts "agent-hub is
	// absent" would pass vacuously if the listing silently failed.
	var mcp MCPStatusListResult
	if err := a.call(ctx, MethodMCPStatusList, map[string]any{}, &mcp); err != nil {
		return caps, fmt.Errorf("mcpServerStatus/list: %w", err)
	}
	caps.MCPTools = map[string]int{}
	for _, s := range mcp.Data {
		caps.MCPServers = append(caps.MCPServers, s.Name)
		caps.MCPTools[s.Name] = len(s.Tools)
	}
	return caps, nil
}

func (a *Adapter) CloseSession(ctx context.Context, sessionID string) error {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	// Call even for a locally absent thread: a prior close or reconnect may
	// have lost bookkeeping while app-server still owns the subscription.
	if err := a.unsubscribe(ctx, sessionID); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.threads[sessionID]; ok {
		st.shut()
		delete(a.threads, sessionID)
	}
	for id, approval := range a.approvals {
		if approval.threadID == sessionID {
			delete(a.approvals, id)
		}
	}
	return nil
}

func (a *Adapter) Close() error {
	a.closeOnce.Do(func() { close(a.done) })
	a.mu.Lock()
	for id, st := range a.threads {
		st.shut()
		delete(a.threads, id)
	}
	c := a.conn
	a.conn = nil
	a.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// watchConn keeps the adapter alive across the death of its child.
//
// When app-server exits, every thread it held is gone. Open turns are
// failed, because the hub's pump would otherwise block forever on a
// channel that will never produce another event, and each session is
// reported cold so the hub stops believing the conversation is live and
// rehydrates it on the next send. Then a fresh child is started: without
// that the adapter stayed dead for the daemon's whole life, and the hub
// process being healthy meant systemd never restarted anything.
func (a *Adapter) watchConn() {
	for {
		c := a.current()
		if c == nil {
			return
		}
		select {
		case <-c.Done():
		case <-a.done:
			return
		}

		a.mu.Lock()
		sessions := make([]*threadState, 0, len(a.threads))
		for id, st := range a.threads {
			sessions = append(sessions, st)
			delete(a.threads, id)
		}
		a.conn = nil
		a.approvals = map[string]pendingApproval{}
		a.mu.Unlock()

		for _, st := range sessions {
			if turnID := st.takeTurn(); turnID != "" {
				st.enqueue(adapter.Event{
					Kind:   adapter.EventTurnFailed,
					TurnID: turnID,
					Err:    "the codex app-server process exited while this turn was running",
					At:     time.Now(),
				}, false)
			}
			// The conversation is not live any more. Saying so is what
			// lets the next peer_send resume the thread from disk.
			st.enqueue(adapter.Event{Kind: adapter.EventSessionCold, At: time.Now()}, false)
			st.shut()
		}
		slog.Warn("codex app-server exited", "open_sessions", len(sessions))

		if !a.reconnect() {
			return
		}
	}
}

// reconnect replaces the child, backing off between attempts. It returns
// false once the adapter is closed or has no way to dial.
func (a *Adapter) reconnect() bool {
	if a.dial == nil {
		return false
	}
	for attempt := 0; ; attempt++ {
		select {
		case <-a.done:
			return false
		case <-time.After(backoffFor(attempt)):
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		conn, err := a.dial(ctx)
		if err == nil {
			err = a.handshake(ctx, conn)
			if err != nil {
				_ = conn.Close()
			}
		}
		cancel()
		if err != nil {
			slog.Warn("could not restart codex app-server",
				"attempt", attempt+1, "err", err)
			continue
		}
		a.mu.Lock()
		a.conn = conn
		a.mu.Unlock()
		slog.Info("codex app-server restarted", "attempts", attempt+1)
		return true
	}
}

// killConnForTest drops the current child the way a crash would.
func (a *Adapter) killConnForTest() {
	if c := a.current(); c != nil {
		_ = c.Close()
	}
}

// pendingApproval remembers how to answer one server-to-client request.
// requested holds the payload the server sent, because two of the five
// response shapes echo back what was asked for rather than a verb.
type pendingApproval struct {
	rpcID     json.RawMessage
	threadID  string
	class     adapter.ApprovalClass
	requested json.RawMessage
}

func (a *Adapter) Steer(ctx context.Context, sessionID, turnID, text string) error {
	return a.call(ctx, MethodTurnSteer, TurnSteerParams{
		ThreadID:       sessionID,
		ExpectedTurnID: turnID,
		Input:          TextInput(text),
	}, nil)
}

func (a *Adapter) Interrupt(ctx context.Context, sessionID, turnID string) error {
	return a.call(ctx, MethodTurnInterrupt, TurnInterruptParams{
		ThreadID: sessionID, TurnID: turnID,
	}, nil)
}

// Approve answers a parked approval request. Decision must be one of
// accept, acceptForSession, decline or cancel.
func (a *Adapter) Approve(_ context.Context, _, requestID string, d adapter.Decision) error {
	switch d.Verb {
	case "accept", "acceptForSession", "decline", "cancel":
	default:
		return fmt.Errorf("codex: %q is not a valid approval decision", d.Verb)
	}
	a.mu.Lock()
	p, ok := a.approvals[requestID]
	delete(a.approvals, requestID)
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("codex: unknown approval request %q", requestID)
	}
	return a.conn.Respond(p.rpcID, approvalResponseBody(p.class, d, p.requested))
}

// approvalResponseBody builds the response for one request class.
//
// The four shapes are taken from the generated schema of codex-cli
// 0.153.4, not guessed. Sending the wrong shape hangs the turn, so if a
// future Codex changes one of these, fix it here rather than working
// around it upstream.
//
//	commandExecution / fileChange  {"decision": <verb>}
//	permissions                    {"permissions": <GrantedPermissionProfile>, "scope": "turn"}
//	tool/requestUserInput          {"answers": {<questionId>: {"answers": [...]}}}
//	mcpServer/elicitation          {"action": "accept"|"decline"|"cancel"}
func approvalResponseBody(class adapter.ApprovalClass, d adapter.Decision, requested json.RawMessage) map[string]any {
	accepted := d.Accepted()

	switch class {
	case adapter.ClassPermissions:
		// RequestPermissionProfile and GrantedPermissionProfile have the
		// same shape, so granting is echoing back what was asked for.
		// Declining grants an empty profile, never a partial one.
		body := map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		if !accepted {
			return body
		}
		var asked struct {
			Permissions json.RawMessage `json:"permissions"`
		}
		if json.Unmarshal(requested, &asked) == nil && len(asked.Permissions) > 0 {
			body["permissions"] = asked.Permissions
		}
		if d.Verb == "acceptForSession" {
			body["scope"] = "session"
		}
		return body

	case adapter.ClassUserInput:
		// Every question must appear in the answers map or the turn stays
		// blocked. The caller's text answers all of them; without one,
		// each question gets an empty list, which Codex reads as "no
		// selection".
		reply := []string{}
		if d.Answer != "" {
			reply = []string{d.Answer}
		}
		answers := map[string]any{}
		var asked struct {
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		}
		if json.Unmarshal(requested, &asked) == nil {
			for _, q := range asked.Questions {
				answers[q.ID] = map[string]any{"answers": reply}
			}
		}
		return map[string]any{"answers": answers}

	case adapter.ClassElicitation:
		action := "decline"
		if accepted {
			action = "accept"
		} else if d.Verb == "cancel" {
			action = "cancel"
		}
		body := map[string]any{"action": action}
		if accepted && d.Answer != "" {
			body["content"] = map[string]any{"answer": d.Answer}
		}
		return body

	default:
		return map[string]any{"decision": d.Verb}
	}
}

// activityLabel turns a thread item into one short line saying what the
// peer is doing. An item that says nothing useful returns "".
func activityLabel(it TurnItem) string {
	switch it.Type {
	case "commandExecution":
		if it.Command != "" {
			return "running " + firstLine(it.Command)
		}
		return "running a command"
	case "fileChange":
		return "editing files"
	case "mcpToolCall":
		if it.Server != "" {
			return "calling " + it.Server
		}
		return "calling an MCP tool"
	case "webSearch":
		if it.Query != "" {
			return "searching the web for " + firstLine(it.Query)
		}
		return "searching the web"
	case "imageView":
		return "viewing " + firstLine(it.Path)
	case "reasoning":
		return "thinking"
	case "plan":
		return "planning"
	case "contextCompaction":
		return "compacting its context"
	case "subAgentActivity":
		if it.Kind != "" {
			return "subagent: " + it.Kind
		}
		return "running a subagent"
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

func (a *Adapter) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	var class adapter.ApprovalClass
	switch method {
	case ReqCommandApproval:
		class = adapter.ClassCommand
	case ReqFileChangeApproval:
		class = adapter.ClassFileChange
	case ReqPermissionApproval:
		class = adapter.ClassPermissions
	case ReqToolUserInput:
		class = adapter.ClassUserInput
	case ReqMCPElicitation:
		class = adapter.ClassElicitation
	default:
		// An unrecognised request must still be answered or the turn
		// hangs forever. Decline it and record why.
		slog.Warn("declining an unrecognised app-server request", "method", method)
		_ = a.conn.Respond(id, map[string]any{"decision": "decline"})
		return
	}

	req := &adapter.ApprovalRequest{ID: NewApprovalID(), Class: class, Raw: params}
	var threadID string

	switch class {
	case adapter.ClassCommand:
		var p CommandApprovalParams
		_ = json.Unmarshal(params, &p)
		threadID, req.Command, req.Cwd, req.Reason = p.ThreadID, p.Command, p.Cwd, p.Reason
		if len(p.ProposedExecpolicyAmendment) > 0 || len(p.ProposedNetworkPolicyAmendments) > 0 {
			req.Class = adapter.ClassPolicy
		}
	case adapter.ClassFileChange:
		var p FileChangeApprovalParams
		_ = json.Unmarshal(params, &p)
		threadID, req.Reason = p.ThreadID, p.Reason
		if p.GrantRoot != "" {
			req.Paths = []string{p.GrantRoot}
		}
	case adapter.ClassPermissions, adapter.ClassUserInput, adapter.ClassElicitation:
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Cwd      string `json:"cwd"`
			Reason   string `json:"reason"`
			Message  string `json:"message"`
		}
		_ = json.Unmarshal(params, &p)
		threadID, req.Cwd, req.Reason = p.ThreadID, p.Cwd, p.Reason
		if req.Reason == "" {
			req.Reason = p.Message
		}
	}

	a.mu.Lock()
	a.approvals[req.ID] = pendingApproval{
		rpcID: id, threadID: threadID, class: class, requested: params}
	a.mu.Unlock()

	var turnID struct {
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &turnID)

	a.emit(threadID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: turnID.TurnID,
		Approval: req, Raw: params,
	})
}

// NewApprovalID returns an opaque identifier for one parked approval.
func NewApprovalID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("codex: entropy unavailable: %v", err))
	}
	return "ap_" + hex.EncodeToString(b[:])
}

var _ adapter.Adapter = (*Adapter)(nil)
