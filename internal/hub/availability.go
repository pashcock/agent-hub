package hub

import (
	"context"
	"fmt"
	"time"
)

func (h *Hub) requireAvailable(ctx context.Context, agent string) error {
	if _, err := h.adapterFor(agent); err != nil {
		return err
	}
	p, err := h.st.ProviderPause(ctx, agent)
	if err != nil {
		return fmt.Errorf("cannot read %s availability: %w", agent, err)
	}
	if p.Paused {
		return fmt.Errorf("hub: %s is paused by the operator: %s; wait for resume, do not retry or switch providers automatically", agent, p.Reason)
	}
	return nil
}

// SetProviderPause gates future turns and queue promotion. Active turns,
// approval answers, cancellation and transcripts remain usable.
func (h *Hub) SetProviderPause(ctx context.Context, agent, reason string, until time.Time, paused bool) error {
	if _, err := h.adapterFor(agent); err != nil {
		return err
	}
	if paused && !until.IsZero() && !until.After(time.Now()) {
		return fmt.Errorf("pause deadline must be in the future")
	}
	h.admission.Lock()
	defer h.admission.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.st.SetProviderPause(ctx, agent, reason, until, paused); err != nil {
		return err
	}
	h.kick()
	return nil
}
