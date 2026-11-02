package codex

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func (a *Adapter) registerOptions(id string, opts adapter.StartOptions) {
	a.register(id)
	a.mu.RLock()
	st := a.threads[id]
	a.mu.RUnlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	st.options, st.cold, st.lastUsed = opts, false, time.Now()
	st.mu.Unlock()
}

// unsubscribe releases the app-server subscription without deleting history.
// App-server owns the subsequent unload grace period and child cleanup.
func (a *Adapter) unsubscribe(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	a.mu.RLock()
	st := a.threads[id]
	a.mu.RUnlock()
	if st != nil {
		st.mu.Lock()
		turn := st.openTurn
		st.mu.Unlock()
		if turn != "" {
			if err := a.Interrupt(ctx, id, turn); err != nil {
				return fmt.Errorf("interrupt before unsubscribe: %w", err)
			}
		}
	}
	var res struct {
		Status string `json:"status"`
	}
	if err := a.call(ctx, "thread/unsubscribe", map[string]string{"threadId": id}, &res); err != nil {
		return fmt.Errorf("thread/unsubscribe: %w", err)
	}
	switch res.Status {
	case "unsubscribed", "notSubscribed", "notLoaded":
		return nil
	default:
		return fmt.Errorf("thread/unsubscribe: unexpected status %q", res.Status)
	}
}

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
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	select {
	case <-a.done:
		return
	default:
	}
	a.mu.RLock()
	sessions := make(map[string]*threadState, len(a.threads))
	for id, st := range a.threads {
		pending := false
		for _, approval := range a.approvals {
			if approval.threadID == id {
				pending = true
				break
			}
		}
		if !pending {
			sessions[id] = st
		}
	}
	a.mu.RUnlock()
	for id, st := range sessions {
		st.mu.Lock()
		eligible := !st.closed && !st.cold && st.openTurn == "" && time.Since(st.lastUsed) >= a.opts.IdleKill
		st.mu.Unlock()
		if !eligible {
			continue
		}
		if err := a.unsubscribe(context.Background(), id); err != nil {
			slog.Warn("could not release idle codex thread", "thread", id, "err", err)
			continue
		}
		st.mu.Lock()
		st.cold = true
		st.mu.Unlock()
		st.enqueue(adapter.Event{Kind: adapter.EventSessionCold, At: time.Now()}, false)
	}
}
