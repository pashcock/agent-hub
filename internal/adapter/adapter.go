// Package adapter defines the narrow contract every native coding agent
// backend implements. It covers session and turn lifecycle only; it does
// not try to unify what the models themselves can do. Provider-specific
// capability is reported through Capabilities and carried through in the
// raw payload of every event.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrUnsupported is returned by an adapter asked for a capability it does
// not have, for example steering a Claude turn.
var ErrUnsupported = errors.New("adapter: unsupported capability")

// ErrSessionGone is returned when a provider no longer recognises a
// session id. The hub translates this to the stale liveness state.
var ErrSessionGone = errors.New("adapter: session gone")

type Mode string

const (
	ModeAsk    Mode = "ask"
	ModePlan   Mode = "plan"
	ModeReview Mode = "review"
	ModeCode   Mode = "code"
)

// ReadOnly reports whether a mode forbids writes.
func (m Mode) ReadOnly() bool { return m != ModeCode }

type StartOptions struct {
	Cwd         string
	Mode        Mode
	Model       string
	Effort      string
	Sandbox     string // empty means "derive from Mode"
	ServiceTier string
	Personality string
}

type TurnOptions struct {
	Model  string
	Effort string
}

type EventKind string

const (
	EventTurnStarted   EventKind = "turn_started"
	EventMessageDelta  EventKind = "message_delta"
	EventItemCompleted EventKind = "item_completed"
	EventTurnCompleted EventKind = "turn_completed"
	EventTurnFailed    EventKind = "turn_failed"
	EventInterrupted   EventKind = "turn_interrupted"
	EventApproval      EventKind = "approval_requested"
	EventTokenUsage    EventKind = "token_usage"
	EventProviderRaw   EventKind = "provider_raw"
	// EventSessionCold reports that the adapter released the provider
	// process for a session while keeping the session itself resumable.
	// The hub owns liveness, so this is how an adapter tells it that a
	// conversation is no longer live.
	EventSessionCold EventKind = "session_cold"
	// EventActivity carries a short label of what the peer is doing right
	// now: the command it is running, the file it is editing, the tool it
	// called. Poll reports the most recent one while a turn runs, which
	// is what turns "silent for 812s" into something a caller can judge.
	EventActivity EventKind = "activity"
)

// Profile is what an adapter knows about itself without contacting the
// provider. peer_list uses it, so it must never spawn a process or make a
// network call.
type Profile struct {
	// Steer reports whether input is absorbed by a running turn.
	Steer bool
	// SandboxedReadOnly reports whether the provider enforces read-only
	// itself, rather than the hub denying tools.
	SandboxedReadOnly bool
	// Approvals reports whether the provider raises permission requests
	// to its client, so peer_approve can answer them.
	Approvals bool
}

// ApprovalClass groups a request for the human-gating rules.
type ApprovalClass string

const (
	ClassCommand     ApprovalClass = "command"
	ClassFileChange  ApprovalClass = "file_change"
	ClassPermissions ApprovalClass = "permissions"
	ClassPolicy      ApprovalClass = "policy_amendment"
	ClassUserInput   ApprovalClass = "user_input"
	ClassElicitation ApprovalClass = "elicitation"
)

// Decision answers a parked permission request.
//
// Answer carries free text for the request kinds that ask a question
// rather than for permission. A verb alone cannot answer "which
// environment?", which is why those used to be replied to with an empty
// selection.
type Decision struct {
	Verb   string
	Answer string
}

func (d Decision) Accepted() bool {
	return d.Verb == "accept" || d.Verb == "acceptForSession"
}

type ApprovalRequest struct {
	ID      string
	Class   ApprovalClass
	Command string
	Paths   []string
	Cwd     string
	Reason  string
	Raw     json.RawMessage
}

// Usage is what one turn consumed, normalized across providers.
//
// Providers report different things: Codex gives token counts only,
// Claude gives counts plus a dollar figure. A zero CostUSD therefore
// means "not reported", not "free".
type Usage struct {
	Model           string
	InputTokens     int
	OutputTokens    int
	CachedInput     int
	CacheWrite      int
	ReasoningTokens int
	TotalTokens     int
	CostUSD         float64
	DurationMS      int
}

// Empty reports whether nothing was measured.
func (u Usage) Empty() bool {
	return u.TotalTokens == 0 && u.InputTokens == 0 && u.OutputTokens == 0 && u.CostUSD == 0
}

// Event is the normalized form of a provider event. Raw always carries the
// untouched provider payload so nothing is lost by normalization.
type Event struct {
	Kind     EventKind
	TurnID   string
	Text     string
	Err      string
	Approval *ApprovalRequest
	// Activity is a short label of what the peer is doing, set on
	// EventActivity.
	Activity string
	// Usage is set on the events that carry a measurement: a provider
	// token-usage notification, or a terminal turn that reports one.
	Usage *Usage
	Raw   json.RawMessage
	At    time.Time
}

type Model struct {
	ID string
	// ResolvedModel is the canonical model behind a native alias, when reported.
	ResolvedModel string
	DefaultEffort string
	Efforts       []string
}

type Capabilities struct {
	// Steer reports whether the adapter can inject text into a running
	// turn. Codex can; Claude queues instead.
	Steer bool
	// SandboxedReadOnly reports whether read-only is enforced by the
	// provider rather than by denying tools.
	SandboxedReadOnly bool
	Models            []Model
	// MCPServers lists the configured server names.
	MCPServers []string
	// MCPTools counts the tools each server actually exposes. A masked
	// server keeps its name in MCPServers but reports zero tools, so this
	// is the field that says what a peer can really call.
	MCPTools map[string]int
	// MCPConnected reports whether each server connected. Claude's init
	// line reports connection, not tool counts, so MCPTools stays empty
	// for providers that cannot count tools.
	MCPConnected map[string]bool
}

// Adapter drives one native agent backend.
//
// Events returns a channel per session. The channel is closed by
// CloseSession or Close. Implementations must not block on a slow reader:
// they buffer and drop EventMessageDelta first if a consumer falls behind.
type Adapter interface {
	Name() string
	// Profile answers from static knowledge. It must not block.
	Profile() Profile

	// Capacity reports how many sessions currently hold a provider
	// process and how many the adapter will hold at once. A limit of 0
	// means unlimited, which is what a backend that multiplexes many
	// sessions into one process reports.
	//
	// The hub gates on this so a caller is refused before a process is
	// spawned, rather than after. It must not block.
	Capacity() (used, limit int)
	Capabilities(ctx context.Context, cwd string) (Capabilities, error)

	StartSession(ctx context.Context, opts StartOptions) (sessionID string, err error)
	ResumeSession(ctx context.Context, sessionID string, opts StartOptions) error

	SendTurn(ctx context.Context, sessionID, text string, opts TurnOptions) (turnID string, err error)
	Steer(ctx context.Context, sessionID, turnID, text string) error
	Interrupt(ctx context.Context, sessionID, turnID string) error
	Approve(ctx context.Context, sessionID, requestID string, d Decision) error

	Events(sessionID string) (<-chan Event, error)

	CloseSession(ctx context.Context, sessionID string) error
	Close() error
}
