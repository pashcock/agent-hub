// Package fake is an in-memory Adapter for testing the hub and the MCP
// facade without any provider binary.
package fake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

type session struct {
	opts adapter.StartOptions
	ch   chan adapter.Event
	n    int
}

type Adapter struct {
	mu             sync.Mutex
	sessions       map[string]*session
	nextID         int
	failNextResume bool
	// Capability lets a test control what the fake reports.
	Capability adapter.Capabilities
	// Prof is what Profile returns; tests set it to whatever they need.
	Prof adapter.Profile
	// Limit is the capacity ceiling; 0 means unlimited.
	Limit int
}

func New() *Adapter {
	return &Adapter{
		sessions: map[string]*session{},
		Prof:     adapter.Profile{Steer: true, SandboxedReadOnly: true, Approvals: true},
		Capability: adapter.Capabilities{
			Steer:             true,
			SandboxedReadOnly: true,
			Models: []adapter.Model{
				{ID: "fake-model", DefaultEffort: "medium", Efforts: []string{"low", "medium", "high"}},
			},
		},
	}
}

func (a *Adapter) Name() string { return "fake" }

func (a *Adapter) Profile() adapter.Profile { return a.Prof }

func (a *Adapter) Capacity() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions), a.Limit
}

func (a *Adapter) Capabilities(context.Context, string) (adapter.Capabilities, error) {
	return a.Capability, nil
}

func (a *Adapter) StartSession(_ context.Context, opts adapter.StartOptions) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	// The suffix keeps ids unique across adapter instances. Two fakes
	// sharing one database would otherwise mint the same turn ids and
	// collide on the turns primary key, which looks like a hub bug.
	id := fmt.Sprintf("fake-session-%d-%s", a.nextID, randSuffix())
	a.sessions[id] = &session{opts: opts, ch: make(chan adapter.Event, 64)}
	return id, nil
}

// FailNextResume makes exactly the next ResumeSession call report a gone session.
func (a *Adapter) FailNextResume() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failNextResume = true
}

func (a *Adapter) ResumeSession(_ context.Context, sessionID string, opts adapter.StartOptions) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failNextResume {
		a.failNextResume = false
		return adapter.ErrSessionGone
	}
	if _, ok := a.sessions[sessionID]; !ok {
		a.sessions[sessionID] = &session{opts: opts, ch: make(chan adapter.Event, 64)}
	}
	return nil
}

func (a *Adapter) get(sessionID string) (*session, error) {
	s, ok := a.sessions[sessionID]
	if !ok {
		return nil, adapter.ErrSessionGone
	}
	return s, nil
}

func (a *Adapter) SendTurn(_ context.Context, sessionID, text string, opts adapter.TurnOptions) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.get(sessionID)
	if err != nil {
		return "", err
	}
	s.n++
	turnID := fmt.Sprintf("%s-turn-%d", sessionID, s.n)
	a.send(s, adapter.Event{Kind: adapter.EventTurnStarted, TurnID: turnID, At: time.Now()})
	return turnID, nil
}

func (a *Adapter) Steer(_ context.Context, sessionID, _, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.get(sessionID)
	return err
}

func (a *Adapter) Interrupt(_ context.Context, sessionID, turnID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.get(sessionID)
	if err != nil {
		return err
	}
	a.send(s, adapter.Event{Kind: adapter.EventInterrupted, TurnID: turnID, At: time.Now()})
	return nil
}

func (a *Adapter) Approve(_ context.Context, sessionID, _ string, _ adapter.Decision) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.get(sessionID)
	return err
}

func (a *Adapter) Events(sessionID string) (<-chan adapter.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.get(sessionID)
	if err != nil {
		return nil, err
	}
	return s.ch, nil
}

// Emit pushes an event into a session's stream. Tests use it to script
// provider behaviour.
func (a *Adapter) Emit(sessionID string, ev adapter.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.get(sessionID)
	if err != nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	a.send(s, ev)
}

// send must be called with the mutex held. It never blocks.
func (a *Adapter) send(s *session, ev adapter.Event) {
	select {
	case s.ch <- ev:
	default:
	}
}

func (a *Adapter) CloseSession(_ context.Context, sessionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[sessionID]
	if !ok {
		return nil
	}
	close(s.ch)
	delete(a.sessions, sessionID)
	return nil
}

func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, s := range a.sessions {
		close(s.ch)
		delete(a.sessions, id)
	}
	return nil
}

var _ adapter.Adapter = (*Adapter)(nil)

func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("fake: entropy unavailable")
	}
	return hex.EncodeToString(b[:])
}
