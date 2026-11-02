// Package hub owns conversation and turn state. It is the only place that
// decides when a native session is rehydrated.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/store"
)

// Reasons a turn ended for something other than the peer's own doing.
// They are sentences rather than enum tags because the only place they
// surface is a tool result read by a model, which has to decide what to
// do next from the text alone.
const (
	ReasonHubRestart = "the hub restarted while this turn was running, so it was " +
		"aborted part-way. Any changes it had already written are still on disk, " +
		"so check the working tree before assuming nothing happened. The " +
		"conversation itself survived: peer_send continues the same native session " +
		"with its context intact."

	ReasonStreamClosed = "the peer process ended without finishing this turn. " +
		"The conversation survived: peer_send continues the same native session."

	ReasonConversationClosed = "the conversation was closed while this turn was running."
)

type Config struct {
	MaxParallelPeerTurns int
	HumanGated           []string
	// ApprovalTimeout bounds how long a parked request may hold a turn
	// and its concurrency slot. Zero means ten minutes.
	ApprovalTimeout time.Duration
	// OnApprovalTimeout is "deny" or "accept".
	OnApprovalTimeout string
	// StallWarn is how long a running turn may produce no events before
	// Poll reports it as stalled. Zero means ten minutes.
	//
	// A stall is reported, never enforced. A legitimate turn can think
	// for an hour; killing it on a timer would be worse than the silence.
	// What the caller needs is to be told, which is exactly what a
	// separate liveness monitor used to do by hand.
	StallWarn time.Duration
}

type StartRequest struct {
	Agent     string
	Task      string
	Cwd       string
	Mode      string
	Model     string
	Effort    string
	Sandbox   string
	Context   string
	CallerKey string
	// Queue asks the hub to wait for room instead of refusing when the
	// agent is at capacity. Without it a full agent is an error, so a
	// caller is never left believing work started when it has not.
	Queue bool
}

type StartResult struct {
	ConversationID  string
	TurnID          string
	NativeSessionID string
	Status          string
}

type PendingApproval struct {
	ID             string
	ConversationID string
	TurnID         string
	Class          string
	Command        string
	Paths          []string
	Cwd            string
	Reason         string
	HumanGated     bool
}

type PollResult struct {
	Agent           string
	Conversation    *ConversationAccounting
	Timings         *store.Timings
	ConversationID  string
	Liveness        string
	TurnID          string
	TurnStatus      string
	Result          string
	Error           string
	Events          []store.Event
	Cursor          int64
	PendingApproval *PendingApproval
	// SilentForS is how long the running turn has produced no events.
	// Zero when no turn is running.
	SilentForS int
	// Usage is what the turn consumed. It is set only once the turn is
	// terminal: a running total is noise in a conversation between
	// agents, and it is not final until the turn ends.
	Usage *store.Usage
	// Activity is what the peer said it was doing most recently, while a
	// turn is running. Empty when it has not said.
	Activity string
	// Stalled reports that the silence has passed Config.StallWarn. The
	// turn is left running; this is information for the caller, not an
	// enforcement action.
	Stalled bool
}

// reservation is one concurrency slot, owned by exactly one turn. Keying
// by turn rather than counting per caller is what makes release idempotent:
// a duplicate or late terminal event cannot free some other turn's slot.
type reservation struct {
	callerKey string
}

type Hub struct {
	st       *store.Store
	adapters map[string]adapter.Adapter
	cfg      Config

	admission sync.RWMutex // pause waits for in-flight admissions, never for running turns
	mu        sync.Mutex
	pumps     map[string]context.CancelFunc // conversationID -> stop event pump
	approvals map[string]*PendingApproval   // approvalID -> parked request
	timers    map[string]*time.Timer        // approvalID -> deadline
	held      map[string]reservation        // turnID -> slot
	inflight  map[string]int                // callerKey -> slots held
	// convMu serializes turn bookkeeping per conversation so a provider
	// event can never be absorbed before the turn row it refers to exists.
	convMu map[string]*sync.Mutex

	// kicks wakes the queue runner as soon as capacity might have freed,
	// rather than waiting for its next tick.
	kicks     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func New(st *store.Store, adapters map[string]adapter.Adapter, cfg Config) *Hub {
	return constructHub(st, adapters, cfg, true)
}

// NewSnapshot constructs a passive view without adapters or background queue
// processing. The caller should also open the snapshot store read-only.
func NewSnapshot(st *store.Store) *Hub { return constructHub(st, nil, Config{}, false) }

func constructHub(st *store.Store, adapters map[string]adapter.Adapter, cfg Config, processQueue bool) *Hub {
	if cfg.MaxParallelPeerTurns < 1 {
		cfg.MaxParallelPeerTurns = 3
	}
	if cfg.ApprovalTimeout <= 0 {
		cfg.ApprovalTimeout = 10 * time.Minute
	}
	if cfg.OnApprovalTimeout == "" {
		cfg.OnApprovalTimeout = "deny"
	}
	if cfg.StallWarn <= 0 {
		cfg.StallWarn = 10 * time.Minute
	}
	h := &Hub{
		st: st, adapters: adapters, cfg: cfg,
		pumps:     map[string]context.CancelFunc{},
		approvals: map[string]*PendingApproval{},
		timers:    map[string]*time.Timer{},
		held:      map[string]reservation{},
		inflight:  map[string]int{},
		convMu:    map[string]*sync.Mutex{},
		kicks:     make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	if processQueue {
		go h.runQueue()
	}
	return h
}

// lockConversation returns the per-conversation mutex, creating it once.
func (h *Hub) lockConversation(convID string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.convMu[convID]
	if !ok {
		m = &sync.Mutex{}
		h.convMu[convID] = m
	}
	return m
}

func (h *Hub) adapterFor(name string) (adapter.Adapter, error) {
	a, ok := h.adapters[name]
	if !ok {
		return nil, fmt.Errorf("hub: unknown agent %q", name)
	}
	return a, nil
}

func startOptions(c store.Conversation, r StartRequest) adapter.StartOptions {
	mode := adapter.Mode(c.Mode)
	if mode == "" {
		mode = adapter.ModeAsk
	}
	sandbox := r.Sandbox
	if sandbox == "" {
		sandbox = c.Sandbox
	}
	return adapter.StartOptions{
		Cwd: c.ProjectCwd, Mode: mode, Model: c.Model,
		Effort: c.Effort, Sandbox: sandbox,
	}
}

// Start opens a conversation, creates the native session and sends the
// first turn. It returns as soon as the provider acknowledges the turn.
func (h *Hub) Start(ctx context.Context, r StartRequest) (StartResult, error) {
	h.admission.RLock()
	defer h.admission.RUnlock()
	if err := h.requireAvailable(ctx, r.Agent); err != nil {
		return StartResult{}, err
	}

	a, err := h.adapterFor(r.Agent)
	if err != nil {
		return StartResult{}, err
	}

	// Gate before anything is created or spawned. An adapter that runs
	// one process per session has a real ceiling; one that multiplexes
	// reports no limit and is never gated.
	if used, limit := a.Capacity(); limit > 0 && used >= limit {
		convID, qerr := h.enqueue(ctx, r)
		if qerr != nil {
			return StartResult{}, qerr
		}
		if convID == "" {
			return StartResult{}, fmt.Errorf(
				"hub: %s is at capacity (%d of %d live). Retry later, or pass "+
					"queue=true to have the hub start it when a slot frees",
				r.Agent, used, limit)
		}
		return StartResult{ConversationID: convID, Status: store.StatusQueued}, nil
	}

	convID, err := h.st.CreateConversation(ctx, store.Conversation{
		PeerAgent: r.Agent, CallerKey: r.CallerKey, ProjectCwd: r.Cwd,
		Model: r.Model, Effort: r.Effort, Mode: r.Mode, Sandbox: r.Sandbox,
	})
	if err != nil {
		return StartResult{}, err
	}
	return h.startInto(ctx, convID, r)
}

// enqueue records a start for later. It returns "" when the caller did
// not ask to queue, which the gate turns into a refusal.
func (h *Hub) enqueue(ctx context.Context, r StartRequest) (string, error) {
	if !r.Queue {
		return "", nil
	}
	convID, err := h.st.CreateConversation(ctx, store.Conversation{
		PeerAgent: r.Agent, CallerKey: r.CallerKey, ProjectCwd: r.Cwd,
		Model: r.Model, Effort: r.Effort, Mode: r.Mode, Sandbox: r.Sandbox,
	})
	if err != nil {
		return "", err
	}
	if err := h.st.SetConversationStatus(ctx, convID, store.StatusQueued); err != nil {
		return "", err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("hub: encoding a queued start: %w", err)
	}
	if err := h.st.Enqueue(ctx, convID, r.Agent, string(body)); err != nil {
		return "", err
	}
	slog.Info("peer start queued", "conversation", convID, "agent", r.Agent)
	return convID, nil
}

// startInto opens the native session and sends the first turn on a
// conversation row that already exists.
func (h *Hub) startInto(ctx context.Context, convID string, r StartRequest) (StartResult, error) {
	dispatch := time.Now()
	a, err := h.adapterFor(r.Agent)
	if err != nil {
		return StartResult{}, err
	}
	if err := h.reserveSlot(r.CallerKey); err != nil {
		return StartResult{}, err
	}
	attached := false
	defer func() {
		if !attached {
			h.releaseUnattached(r.CallerKey)
		}
	}()

	conv, err := h.st.GetConversation(ctx, convID)
	if err != nil {
		return StartResult{}, err
	}

	sessionID, err := a.StartSession(ctx, startOptions(conv, r))
	if err != nil {
		return StartResult{}, fmt.Errorf("start %s session: %w", r.Agent, err)
	}
	if err := h.st.SetNativeSessionID(ctx, convID, sessionID); err != nil {
		return StartResult{}, err
	}
	if conv.Status != store.StatusLive {
		if err := h.st.SetConversationStatus(ctx, convID, store.StatusLive); err != nil {
			return StartResult{}, err
		}
	}
	h.startPump(convID, sessionID, a)

	text := r.Task
	if r.Context != "" {
		text = r.Task + "\n\n--- context from caller ---\n" + r.Context
	}
	turnID, err := h.sendTurn(ctx, a, convID, sessionID, text,
		adapter.TurnOptions{Model: r.Model, Effort: r.Effort}, conv.CreatedAt, dispatch)
	if err != nil {
		return StartResult{}, err
	}
	h.attachSlot(turnID, r.CallerKey)
	attached = true
	slog.Info("peer turn started",
		"conversation", convID, "agent", r.Agent, "turn", turnID,
		"model", r.Model, "effort", r.Effort, "cwd", r.Cwd)
	return StartResult{
		ConversationID: convID, TurnID: turnID,
		NativeSessionID: sessionID, Status: store.TurnRunning,
	}, nil
}

// sendTurn holds the conversation lock across the provider call and the
// database write.
//
// Without the lock a fast provider can emit turn/completed before
// CreateTurn has run: absorb would then call FinishTurn on a row that does
// not exist yet, the error would be swallowed, and CreateTurn would
// afterwards insert a turn stuck in `running` forever. The pump takes the
// same lock in absorb, so ordering is guaranteed.
func (h *Hub) sendTurn(ctx context.Context, a adapter.Adapter, convID, sessionID, text string, opts adapter.TurnOptions, requested, dispatch time.Time) (string, error) {
	m := h.lockConversation(convID)
	m.Lock()
	defer m.Unlock()

	prompt := time.Now()
	turnID, err := a.SendTurn(ctx, sessionID, text, opts)
	if err != nil {
		return "", fmt.Errorf("send turn: %w", err)
	}
	if _, err := h.st.CreateTurn(ctx, store.Turn{
		ID: turnID, ConversationID: convID, Model: opts.Model, Effort: opts.Effort,
	}); err != nil {
		return "", err
	}
	// SendTurn can transparently reload a process evicted since our earlier
	// liveness check. Update liveness here, not from delayed started events.
	if err := h.st.SetConversationStatus(ctx, convID, store.StatusLive); err != nil {
		return "", err
	}
	if err := h.st.BeginTiming(ctx, turnID, requested, dispatch, prompt, time.Now()); err != nil {
		slog.Error("record timing", "err", err)
	}
	if err := h.st.AppendMessage(ctx, convID, turnID, "user", text); err != nil {
		return "", err
	}
	return turnID, nil
}

// Send continues an existing conversation, rehydrating it if it is cold.
func (h *Hub) Send(ctx context.Context, convID, text string, opts adapter.TurnOptions) (string, error) {
	requested := time.Now()
	h.admission.RLock()
	defer h.admission.RUnlock()

	conv, err := h.st.GetConversation(ctx, convID)
	if err != nil {
		return "", err
	}
	if conv.Status == store.StatusClosed {
		return "", fmt.Errorf("hub: conversation %s is closed", convID)
	}
	if conv.Status == store.StatusStale {
		return "", fmt.Errorf("hub: conversation %s is stale; start a new one", convID)
	}
	if conv.Status == store.StatusQueued {
		return "", fmt.Errorf(
			"hub: conversation %s is still queued and has not started; poll it first",
			convID)
	}
	if err := h.requireAvailable(ctx, conv.PeerAgent); err != nil {
		return "", err
	}
	a, err := h.adapterFor(conv.PeerAgent)
	if err != nil {
		return "", err
	}

	// One turn at a time per conversation. Allowing two would make
	// LatestTurn ambiguous, let two approvals park against the same
	// conversation, and break the turn state machine.
	if turn, err := h.st.LatestTurn(ctx, convID); err == nil {
		if turn.Status == store.TurnRunning || turn.Status == store.TurnQueued ||
			turn.Status == store.TurnNeedsPermission {
			return "", fmt.Errorf(
				"hub: conversation %s already has turn %s in state %s; poll or cancel it first",
				convID, turn.ID, turn.Status)
		}
	}

	if err := h.reserveSlot(conv.CallerKey); err != nil {
		return "", err
	}
	attached := false
	defer func() {
		if !attached {
			h.releaseUnattached(conv.CallerKey)
		}
	}()

	if conv.Status == store.StatusCold {
		if err := h.rehydrate(ctx, conv, a); err != nil {
			return "", err
		}
	}
	if opts.Model == "" {
		opts.Model = conv.Model
	}
	if opts.Effort == "" {
		opts.Effort = conv.Effort
	}
	turnID, err := h.sendTurn(ctx, a, convID, conv.NativeSessionID, text, opts, requested, requested)
	if err != nil {
		return "", err
	}
	h.attachSlot(turnID, conv.CallerKey)
	attached = true
	return turnID, nil
}

func (h *Hub) rehydrate(ctx context.Context, conv store.Conversation, a adapter.Adapter) error {
	err := a.ResumeSession(ctx, conv.NativeSessionID, startOptions(conv, StartRequest{}))
	if err != nil {
		_ = h.st.SetConversationStatus(ctx, conv.ID, store.StatusStale)
		return fmt.Errorf("hub: resuming %s failed, marked stale: %w", conv.ID, err)
	}
	if err := h.st.SetConversationStatus(ctx, conv.ID, store.StatusLive); err != nil {
		return err
	}
	h.startPump(conv.ID, conv.NativeSessionID, a)
	return nil
}

// startPump drains the adapter's event channel into the store.
func (h *Hub) startPump(convID, sessionID string, a adapter.Adapter) {
	h.mu.Lock()
	if _, running := h.pumps[convID]; running {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.pumps[convID] = cancel
	h.mu.Unlock()

	ch, err := a.Events(sessionID)
	if err != nil {
		// No stream means no events will ever arrive, so remove the pump
		// entry rather than leaving a phantom one behind.
		cancel()
		h.mu.Lock()
		delete(h.pumps, convID)
		h.mu.Unlock()
		slog.Warn("no event stream for session", "conversation", convID, "err", err)
		return
	}
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.pumps, convID)
			h.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, open := <-ch:
				if !open {
					// The provider stream ended without a terminal
					// event. Fail whatever was open so the turn does
					// not sit in `running` and hold its slot forever.
					h.failOpenTurn(context.WithoutCancel(ctx), convID, ReasonStreamClosed)
					return
				}
				h.absorb(ctx, convID, ev)
			}
		}
	}()
}

// failOpenTurn terminates a conversation's turn if it is still open.
func (h *Hub) failOpenTurn(ctx context.Context, convID, reason string) {
	m := h.lockConversation(convID)
	m.Lock()
	defer m.Unlock()

	turn, err := h.st.LatestTurn(ctx, convID)
	if err != nil {
		return
	}
	switch turn.Status {
	case store.TurnRunning, store.TurnQueued, store.TurnNeedsPermission:
	default:
		return
	}
	if err := h.st.FinishTurn(ctx, turn.ID, store.TurnFailed, reason); err != nil {
		slog.Error("could not fail an open turn",
			"conversation", convID, "turn", turn.ID, "err", err)
	}
	h.releaseSlot(turn.ID)
	h.dropApprovalsForTurn(turn.ID)
	slog.Warn("turn failed", "conversation", convID, "turn", turn.ID, "reason", reason)
}

func (h *Hub) dropApprovalsForTurn(turnID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, p := range h.approvals {
		if p.TurnID == turnID {
			delete(h.approvals, id)
			if t, ok := h.timers[id]; ok {
				t.Stop()
				delete(h.timers, id)
			}
		}
	}
}

// absorb applies one provider event. It holds the conversation lock so it
// can never observe a turn that sendTurn has not finished persisting.
func (h *Hub) absorb(ctx context.Context, convID string, ev adapter.Event) {
	m := h.lockConversation(convID)
	m.Lock()
	defer m.Unlock()

	if ev.Usage != nil && ev.TurnID != "" {
		if err := h.st.RecordUsage(ctx, convID, ev.TurnID, store.Usage{
			Model: ev.Usage.Model, InputTokens: ev.Usage.InputTokens,
			OutputTokens: ev.Usage.OutputTokens, CachedInput: ev.Usage.CachedInput,
			CacheWrite: ev.Usage.CacheWrite, ReasoningTokens: ev.Usage.ReasoningTokens,
			TotalTokens: ev.Usage.TotalTokens, CostUSD: ev.Usage.CostUSD,
			DurationMS: ev.Usage.DurationMS,
		}); err != nil {
			slog.Error("could not record usage",
				"conversation", convID, "turn", ev.TurnID, "err", err)
		}
	}

	if ev.TurnID != "" {
		if err := h.st.ObserveTiming(ctx, ev.TurnID, string(ev.Kind), time.Now()); err != nil {
			slog.Error("observe timing", "err", err)
		}
	}
	payload := ev.Raw
	if ev.Kind == adapter.EventActivity {
		if b, err := json.Marshal(ev.Activity); err == nil {
			payload = b
		}
	}
	if err := h.st.AppendEvent(ctx, convID, ev.TurnID, string(ev.Kind), payload); err != nil {
		slog.Error("could not persist a provider event",
			"conversation", convID, "kind", ev.Kind, "err", err)
	}

	conv, err := h.st.GetConversation(ctx, convID)
	if err != nil {
		return
	}

	// finish moves the turn to a terminal state and frees its slot. The
	// slot is released only when the transition actually happened, so a
	// duplicate or late terminal event is a no-op rather than an
	// accounting error.
	finish := func(status, errText string) {
		turnID := ev.TurnID
		if turnID == "" {
			if t, err := h.st.LatestTurn(ctx, convID); err == nil {
				turnID = t.ID
			}
		}
		if turnID == "" {
			return
		}
		cur, err := h.st.GetTurn(ctx, turnID)
		if err != nil {
			slog.Error("terminal event for an unknown turn",
				"conversation", convID, "turn", turnID, "err", err)
			return
		}
		switch cur.Status {
		case store.TurnRunning, store.TurnQueued, store.TurnNeedsPermission:
		default:
			return // Already terminal. Ignore the repeat.
		}
		if err := h.st.FinishTurn(ctx, turnID, status, errText); err != nil {
			slog.Error("could not finish a turn",
				"conversation", convID, "turn", turnID, "err", err)
			return
		}
		h.releaseSlot(turnID)
		h.dropApprovalsForTurn(turnID)
		// A finished turn may be what a queued start was waiting for.
		h.kick()
	}

	switch ev.Kind {
	case adapter.EventTurnCompleted:
		if ev.Text != "" {
			if err := h.st.AppendMessage(ctx, convID, ev.TurnID, "assistant", ev.Text); err != nil {
				slog.Error("could not persist a peer reply",
					"conversation", convID, "err", err)
			}
		}
		finish(store.TurnCompleted, "")
		slog.Info("peer turn completed", "conversation", convID, "turn", ev.TurnID)
	case adapter.EventInterrupted:
		finish(store.TurnInterrupted, "")
	case adapter.EventTurnFailed:
		finish(store.TurnFailed, ev.Err)
	case adapter.EventSessionCold:
		// A release event may have queued just before Send reloaded the
		// process. Never apply that stale cold notification to a new turn.
		if latest, err := h.st.LatestTurn(ctx, convID); err == nil &&
			(latest.Status == store.TurnRunning || latest.Status == store.TurnNeedsPermission) {
			break
		}
		// The adapter released the process. Never touch a turn: a
		// well-behaved adapter does not release a busy session, and if
		// one does, failing the turn here would be worse than leaving it.
		if conv.Status == store.StatusLive {
			if err := h.st.SetConversationStatus(ctx, convID, store.StatusCold); err != nil {
				slog.Error("could not mark a released conversation cold",
					"conversation", convID, "err", err)
			}
		}
	case adapter.EventApproval:
		if ev.Approval == nil {
			return
		}
		turnID := ev.TurnID
		if turnID == "" {
			if t, err := h.st.LatestTurn(ctx, convID); err == nil {
				turnID = t.ID
			}
		}
		p := &PendingApproval{
			ID: ev.Approval.ID, ConversationID: convID, TurnID: turnID,
			Class:   string(ev.Approval.Class),
			Command: ev.Approval.Command, Paths: ev.Approval.Paths,
			Cwd: ev.Approval.Cwd, Reason: ev.Approval.Reason,
			HumanGated: humanGated(*ev.Approval, conv.ProjectCwd, h.cfg.HumanGated),
		}
		h.parkApproval(p)
		if err := h.st.SetTurnStatus(ctx, turnID, store.TurnNeedsPermission); err != nil {
			slog.Error("could not park a turn for approval",
				"conversation", convID, "turn", turnID, "err", err)
		}
	}
}

// parkApproval stores a request and arms its deadline. Without the
// deadline an unanswered request holds its turn and its concurrency slot
// forever.
func (h *Hub) parkApproval(p *PendingApproval) {
	timer := time.AfterFunc(h.cfg.ApprovalTimeout, func() {
		h.expireApproval(p.ID)
	})
	h.mu.Lock()
	h.approvals[p.ID] = p
	h.timers[p.ID] = timer
	h.mu.Unlock()
}

func (h *Hub) expireApproval(approvalID string) {
	h.mu.Lock()
	p, ok := h.approvals[approvalID]
	h.mu.Unlock()
	if !ok {
		return
	}
	decision := adapter.Decision{Verb: "decline"}
	if h.cfg.OnApprovalTimeout == "accept" {
		decision.Verb = "accept"
	}
	slog.Warn("approval timed out",
		"approval", approvalID, "conversation", p.ConversationID,
		"decision", decision.Verb, "human_gated", p.HumanGated)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.deliverApproval(ctx, p.ConversationID, approvalID, decision); err != nil {
		slog.Error("could not deliver a timed-out approval",
			"approval", approvalID, "err", err)
	}
}

// humanGated reports whether a request falls into one of the classes the
// orchestrator is not allowed to decide alone.
func humanGated(req adapter.ApprovalRequest, cwd string, gated []string) bool {
	on := func(name string) bool {
		for _, g := range gated {
			if g == name {
				return true
			}
		}
		return false
	}
	if on("policy_amendment") && req.Class == adapter.ClassPolicy {
		return true
	}
	if on("privilege_escalation") && isPrivilegeEscalation(req.Command) {
		return true
	}
	if on("write_outside_cwd") {
		for _, p := range req.Paths {
			if !withinCwd(p, cwd) {
				return true
			}
		}
	}
	return false
}

var escalationBins = []string{"sudo", "su", "pkexec", "doas"}

func isPrivilegeEscalation(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	head := filepath.Base(fields[0])
	for _, b := range escalationBins {
		if head == b {
			return true
		}
	}
	return false
}

// withinCwd reports whether target is cwd or lives inside it. It compares
// cleaned paths segment-wise so /tmp/proj-evil is not inside /tmp/proj.
func withinCwd(target, cwd string) bool {
	if cwd == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(cwd), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Poll reports conversation state. It never rehydrates: a cold
// conversation is answered entirely from the database.
func (h *Hub) Poll(ctx context.Context, convID string, since int64) (PollResult, error) {
	return h.poll(ctx, convID, since, true)
}

// Snapshot reads current state and the latest event cursor, without replaying
// old payloads. MCP Poll retains its incremental event-consumption contract.
func (h *Hub) Snapshot(ctx context.Context, convID string) (PollResult, error) {
	return h.poll(ctx, convID, 0, false)
}
func (h *Hub) poll(ctx context.Context, convID string, since int64, withEvents bool) (PollResult, error) {
	conv, err := h.st.GetConversation(ctx, convID)
	if err != nil {
		return PollResult{}, err
	}
	res := PollResult{Agent: conv.PeerAgent, ConversationID: convID, Liveness: conv.Status, Cursor: since}
	usage, err := h.st.ConversationUsage(ctx, convID)
	if err != nil {
		return PollResult{}, err
	}
	coverage, err := h.st.ConversationCoverage(ctx, convID)
	if err != nil {
		return PollResult{}, err
	}
	res.Conversation = &ConversationAccounting{Usage: ConsumptionOf(conv.PeerAgent, usage), AccountingCoverage: coverage,
		Scope: "reported_hub_turns", SubagentAccounting: "provider_reported_only; child_usage_not_independently_collected; inclusion_unverified"}

	if conv.Status == store.StatusQueued {
		res.TurnStatus = store.TurnQueued
		return res, nil
	}

	turn, err := h.st.LatestTurn(ctx, convID)
	switch {
	case err == store.ErrNotFound:
		// No turn yet; nothing more to report.
	case err != nil:
		return PollResult{}, err
	default:
		res.TurnID = turn.ID
		res.TurnStatus = turn.Status
		res.Error = turn.Error
	}

	if withEvents {
		events, err := h.st.EventsSince(ctx, convID, since, 200)
		if err != nil {
			return PollResult{}, err
		}
		res.Events = events
		if n := len(events); n > 0 {
			res.Cursor = events[n-1].Seq
		}

	} else {
		res.Cursor, err = h.st.EventCursor(ctx, convID)
		if err != nil {
			return PollResult{}, err
		}
	}

	switch res.TurnStatus {
	case store.TurnCompleted, store.TurnFailed, store.TurnInterrupted:
		if u, err := h.st.TurnUsage(ctx, res.TurnID); err == nil && !u.Empty() {
			cp := u
			res.Usage = &cp
		}
	}

	if res.TurnStatus == store.TurnCompleted {
		res.Result, err = h.st.TurnResult(ctx, convID, res.TurnID)
		if err != nil {
			return PollResult{}, err
		}
	}

	if res.TurnStatus == store.TurnRunning || res.TurnStatus == store.TurnQueued {
		if act, err := h.st.LatestActivity(ctx, convID, res.TurnID); err == nil {
			res.Activity = act
		}
		since, err := h.st.LastEventAt(ctx, convID, res.TurnID)
		if err == nil {
			silent := time.Since(since)
			if silent < 0 {
				silent = 0
			}
			res.SilentForS = int(silent / time.Second)
			res.Stalled = silent >= h.cfg.StallWarn
			if res.Stalled {
				slog.Warn("peer turn has gone quiet",
					"conversation", convID, "turn", res.TurnID,
					"silent_for_s", res.SilentForS)
			}
		}
	}

	if res.TurnStatus == store.TurnNeedsPermission {
		// Select by conversation and turn. Picking an arbitrary map entry
		// would hand the caller another conversation's request.
		h.mu.Lock()
		for _, p := range h.approvals {
			if p.ConversationID == convID && (res.TurnID == "" || p.TurnID == res.TurnID) {
				cp := *p
				res.PendingApproval = &cp
				break
			}
		}
		h.mu.Unlock()
	}
	if res.TurnID != "" {
		res.Timings, _ = h.st.TurnTimings(ctx, res.TurnID)
	}
	return res, nil
}

// Transcript returns the observable dialogue. It never rehydrates.
func (h *Hub) Transcript(ctx context.Context, convID string, full bool) ([]store.Message, error) {
	msgs, err := h.st.ListMessages(ctx, convID)
	if err != nil {
		return nil, err
	}
	if full || len(msgs) <= 20 {
		return msgs, nil
	}
	return msgs[len(msgs)-20:], nil
}

func (h *Hub) Steer(ctx context.Context, convID, text string) error {
	conv, a, turn, err := h.runningTurn(ctx, convID)
	if err != nil {
		return err
	}
	if err := a.Steer(ctx, conv.NativeSessionID, turn.ID, text); err != nil {
		// An adapter that cannot steer delivered nothing, so nothing is
		// recorded either.
		return err
	}
	return h.st.AppendMessage(ctx, convID, turn.ID, "user", text)
}

func (h *Hub) Cancel(ctx context.Context, convID string) error {
	// A queued conversation has no turn to interrupt; cancelling it means
	// taking it out of the queue.
	if c, err := h.st.GetConversation(ctx, convID); err == nil && c.Status == store.StatusQueued {
		if err := h.st.Dequeue(ctx, convID); err != nil {
			return err
		}
		return h.st.SetConversationStatus(ctx, convID, store.StatusClosed)
	}

	conv, a, turn, err := h.runningTurn(ctx, convID)
	if err == errNoRunningTurn {
		return nil // Cancelling nothing succeeds.
	}
	if err != nil {
		return err
	}
	return a.Interrupt(ctx, conv.NativeSessionID, turn.ID)
}

var errNoRunningTurn = fmt.Errorf("hub: no running turn")

func (h *Hub) runningTurn(ctx context.Context, convID string) (store.Conversation, adapter.Adapter, store.Turn, error) {
	conv, err := h.st.GetConversation(ctx, convID)
	if err != nil {
		return store.Conversation{}, nil, store.Turn{}, err
	}
	a, err := h.adapterFor(conv.PeerAgent)
	if err != nil {
		return conv, nil, store.Turn{}, err
	}
	turn, err := h.st.LatestTurn(ctx, convID)
	if err != nil {
		return conv, a, store.Turn{}, errNoRunningTurn
	}
	if turn.Status != store.TurnRunning && turn.Status != store.TurnNeedsPermission {
		return conv, a, turn, errNoRunningTurn
	}
	return conv, a, turn, nil
}

// Approve answers a parked request on behalf of the orchestrator model.
// Human-gated classes are refused here by design.
func (h *Hub) Approve(ctx context.Context, convID, approvalID string, d adapter.Decision) error {
	h.mu.Lock()
	p, ok := h.approvals[approvalID]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("hub: unknown approval %q", approvalID)
	}
	if p.ConversationID != convID {
		return fmt.Errorf("hub: approval %q belongs to conversation %s, not %s",
			approvalID, p.ConversationID, convID)
	}
	if p.HumanGated {
		return fmt.Errorf(
			"hub: approval %s is human-gated (%s); the operator must answer with `agent-hub approve %s`",
			approvalID, p.Class, approvalID)
	}
	return h.deliverApproval(ctx, convID, approvalID, d)
}

// ApproveAsOperator bypasses the human gate. Only the local CLI and the
// web view call it.
func (h *Hub) ApproveAsOperator(ctx context.Context, convID, approvalID string, d adapter.Decision) error {
	return h.deliverApproval(ctx, convID, approvalID, d)
}

func (h *Hub) deliverApproval(ctx context.Context, convID, approvalID string, d adapter.Decision) error {
	h.mu.Lock()
	p, ok := h.approvals[approvalID]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("hub: unknown approval %q", approvalID)
	}
	// The hub already knows which conversation a parked request belongs
	// to, so an operator answering from a terminal does not have to
	// repeat it. A caller that does name one must name the right one.
	switch {
	case convID == "":
		convID = p.ConversationID
	case convID != p.ConversationID:
		return fmt.Errorf("hub: approval %q belongs to conversation %s, not %s",
			approvalID, p.ConversationID, convID)
	}
	conv, err := h.st.GetConversation(ctx, convID)
	if err != nil {
		return err
	}
	a, err := h.adapterFor(conv.PeerAgent)
	if err != nil {
		return err
	}
	if err := a.Approve(ctx, conv.NativeSessionID, approvalID, d); err != nil {
		return err
	}
	h.mu.Lock()
	delete(h.approvals, approvalID)
	if t, ok := h.timers[approvalID]; ok {
		t.Stop()
		delete(h.timers, approvalID)
	}
	h.mu.Unlock()
	// Restore the turn this request parked, not merely the latest one.
	if p.TurnID != "" {
		_ = h.st.EndApprovalTiming(ctx, p.TurnID, time.Now())
		if err := h.st.SetTurnStatus(ctx, p.TurnID, store.TurnRunning); err != nil {
			slog.Error("could not resume a turn after approval",
				"turn", p.TurnID, "err", err)
		}
	}
	return nil
}

// PendingApprovals lists parked requests for the operator CLI.
func (h *Hub) PendingApprovals() []PendingApproval {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]PendingApproval, 0, len(h.approvals))
	for _, p := range h.approvals {
		out = append(out, *p)
	}
	return out
}

func (h *Hub) CloseConversation(ctx context.Context, convID string) error {
	// Finish in-flight admission before closing the native session, and keep
	// sends/queue dispatch from resuming it before the closed status is stored.
	h.admission.Lock()
	defer h.admission.Unlock()
	conv, err := h.st.GetConversation(ctx, convID)
	if err != nil {
		return err
	}
	if a, err := h.adapterFor(conv.PeerAgent); err == nil {
		if err := a.CloseSession(ctx, conv.NativeSessionID); err != nil {
			return fmt.Errorf("close peer session: %w", err)
		}
	}
	// Do not free the slot or discard queued work while provider cleanup
	// failed: the peer may still be running and closure must remain retryable.
	h.failOpenTurn(ctx, convID, ReasonConversationClosed)
	_ = h.st.Dequeue(ctx, convID)

	h.mu.Lock()
	if cancel, ok := h.pumps[convID]; ok {
		cancel()
	}
	h.mu.Unlock()
	if err := h.st.SetConversationStatus(ctx, convID, store.StatusClosed); err != nil {
		return err
	}
	h.kick()
	return nil
}

// Recover is called once at daemon startup, before serving.
func (h *Hub) Recover(ctx context.Context) error {
	if err := h.st.FailRunningTurns(ctx, ReasonHubRestart); err != nil {
		return err
	}
	h.mu.Lock()
	h.held = map[string]reservation{}
	h.inflight = map[string]int{}
	for id, t := range h.timers {
		t.Stop()
		delete(h.timers, id)
	}
	h.approvals = map[string]*PendingApproval{}
	h.mu.Unlock()
	if err := h.st.MarkAllCold(ctx); err != nil {
		return err
	}
	h.kick()
	return nil
}

// reserveSlot takes a concurrency slot for a caller. The slot is not yet
// attached to a turn, because the provider assigns the turn id.
func (h *Hub) reserveSlot(callerKey string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight[callerKey] >= h.cfg.MaxParallelPeerTurns {
		return fmt.Errorf("hub: caller already has %d peer turns running (limit %d)",
			h.inflight[callerKey], h.cfg.MaxParallelPeerTurns)
	}
	h.inflight[callerKey]++
	return nil
}

// attachSlot binds an already-reserved slot to the turn that got it.
func (h *Hub) attachSlot(turnID, callerKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[turnID] = reservation{callerKey: callerKey}
}

// releaseSlot frees a slot. It is idempotent: releasing a turn whose slot
// is already gone does nothing, so a duplicate or late terminal event can
// never free a different turn's slot.
func (h *Hub) releaseSlot(turnID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseSlotLocked(turnID)
}

func (h *Hub) releaseSlotLocked(turnID string) {
	r, ok := h.held[turnID]
	if !ok {
		return
	}
	delete(h.held, turnID)
	if h.inflight[r.callerKey] > 0 {
		h.inflight[r.callerKey]--
	}
}

// releaseUnattached frees a slot that was reserved but never bound to a
// turn, for example when SendTurn itself failed.
func (h *Hub) releaseUnattached(callerKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight[callerKey] > 0 {
		h.inflight[callerKey]--
	}
}

func (h *Hub) Close() error {
	h.closeOnce.Do(func() { close(h.done) })
	h.mu.Lock()
	for id, cancel := range h.pumps {
		cancel()
		delete(h.pumps, id)
	}
	for id, t := range h.timers {
		t.Stop()
		delete(h.timers, id)
	}
	h.mu.Unlock()
	return nil
}

// AgentStatus is what one agent looks like right now.
type AgentStatus struct {
	Availability store.ProviderPause `json:"availability"`
	Name         string              `json:"name"`
	Profile      adapter.Profile     `json:"profile"`
	Used         int                 `json:"used"`
	Limit        int                 `json:"limit"`
}

// AgentStatuses reports every configured agent. It never contacts a
// provider, so it is safe to call on every page render.
func (h *Hub) AgentStatuses() []AgentStatus {
	out := make([]AgentStatus, 0, len(h.adapters))
	for name, a := range h.adapters {
		used, limit := a.Capacity()
		pause, err := h.st.ProviderPause(context.Background(), name)
		if err != nil {
			pause = store.ProviderPause{Paused: true, Reason: "cannot read availability"}
		}
		out = append(out, AgentStatus{
			Name: name, Profile: a.Profile(), Used: used, Limit: limit, Availability: pause})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Profile reports what an agent supports, without contacting it.
func (h *Hub) Profile(agent string) (adapter.Profile, error) {
	a, err := h.adapterFor(agent)
	if err != nil {
		return adapter.Profile{}, err
	}
	return a.Profile(), nil
}

// Capabilities reports what a peer agent supports.
func (h *Hub) Capabilities(ctx context.Context, agent, cwd string) (adapter.Capabilities, error) {
	if err := h.requireAvailable(ctx, agent); err != nil {
		return adapter.Capabilities{}, err
	}
	a, err := h.adapterFor(agent)
	if err != nil {
		return adapter.Capabilities{}, err
	}
	return a.Capabilities(ctx, cwd)
}
