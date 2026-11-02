package grok

import (
	"github.com/pashcock/agent-hub/internal/adapter"
	"time"
)

func (a *Adapter) sweepIdle() {
	interval := a.opts.IdleKill / 4
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			a.evictIdle()
		}
	}
}

func (a *Adapter) evictIdle() {
	a.mu.Lock()
	sessions := make([]*session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		if s.closed || s.proc == nil || s.turn != "" || len(s.approvals) > 0 || s.lastUsed.IsZero() || time.Since(s.lastUsed) < a.opts.IdleKill {
			s.mu.Unlock()
			continue
		}
		r := s.proc
		s.proc = nil
		// Enqueue while locked: a resumed turn's events must follow this one.
		s.q.put(adapter.Event{Kind: adapter.EventSessionCold, At: time.Now()})
		s.mu.Unlock()
		r.close()
	}
}
