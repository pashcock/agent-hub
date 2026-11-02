package claude

import (
	"sort"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

// LiveProcesses counts sessions holding a process, plus any capability
// probe in flight. A probe is a real 283 MB process and must count
// against the limit like any other.
func (a *Adapter) LiveProcesses() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	n := a.probes
	for _, s := range a.sessions {
		s.mu.Lock()
		if s.proc != nil {
			n++
		}
		s.mu.Unlock()
	}
	return n
}

// Capacity reports live processes against the configured ceiling. This is
// what lets the hub refuse a caller before spawning a 283 MB process,
// instead of spawning it and then discovering the cache is over budget.
func (a *Adapter) Capacity() (int, int) {
	return a.LiveProcesses(), a.opts.MaxLiveProcesses
}

func (a *Adapter) isLive(sessionID string) bool {
	s, err := a.get(sessionID)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc != nil
}

// evictIfNeeded releases processes that are over the limit or past the
// idle deadline. keep names a session that must not be touched, which is
// the one the caller just created.
func (a *Adapter) evictIfNeeded(keep string) {
	type candidate struct {
		s        *session
		lastUsed time.Time
	}
	a.mu.RLock()
	live := a.probes
	var idle []candidate
	for id, s := range a.sessions {
		s.mu.Lock()
		hasProc := s.proc != nil
		busy := len(s.pending) > 0
		last := s.lastUsed
		s.mu.Unlock()
		if !hasProc {
			continue
		}
		live++
		if busy || id == keep {
			continue
		}
		idle = append(idle, candidate{s: s, lastUsed: last})
	}
	a.mu.RUnlock()

	sort.Slice(idle, func(i, j int) bool { return idle[i].lastUsed.Before(idle[j].lastUsed) })

	now := time.Now()
	for _, c := range idle {
		overLimit := live > a.opts.MaxLiveProcesses
		tooOld := now.Sub(c.lastUsed) >= a.opts.IdleKill
		if !overLimit && !tooOld {
			continue
		}
		reason := "idle timeout"
		if overLimit {
			reason = "over the live-process limit"
		}
		if a.release(c.s) {
			live--
			a.logEviction(c.s.id, reason)
		}
	}
}

// release drops a session's process while keeping the session, so the
// next send can bring it back with --resume. It returns false if the
// session became busy in the meantime.
//
// The hub owns liveness, so a successful release is announced. Without
// that, peer_poll would keep reporting a conversation live with no
// process behind it.
func (a *Adapter) release(s *session) bool {
	s.mu.Lock()
	if s.proc == nil || len(s.pending) > 0 {
		s.mu.Unlock()
		return false
	}
	p := s.proc
	s.proc = nil
	s.gen++ // the running pump is now stale and will exit quietly
	s.mu.Unlock()

	p.Close()
	s.enqueue(adapter.Event{Kind: adapter.EventSessionCold, At: time.Now()}, false)
	return true
}

// sweepIdle releases sessions that go quiet, without waiting for
// unrelated traffic to trigger a check.
func (a *Adapter) sweepIdle() {
	interval := a.opts.IdleKill / 4
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-t.C:
			a.evictIfNeeded("")
		}
	}
}
