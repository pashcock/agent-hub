package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/pashcock/agent-hub/internal/store"
)

// queueTick bounds how long a queued start waits after a slot frees when
// no kick arrives. Terminal turns and closes kick the runner directly, so
// this is only a backstop.
const queueTick = 250 * time.Millisecond

// kick wakes the queue runner without blocking. A pending wake-up already
// covers a second one, so a full channel is success, not a failure.
func (h *Hub) kick() {
	select {
	case h.kicks <- struct{}{}:
	default:
	}
}

func (h *Hub) runQueue() {
	t := time.NewTicker(queueTick)
	defer t.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-h.kicks:
		case <-t.C:
		}
		h.promoteQueued(context.Background())
	}
}

// promoteQueued starts the oldest waiting conversation of every agent
// that now has room. It takes them one at a time: capacity is re-read
// after each start, so a single free slot never releases two.
func (h *Hub) promoteQueued(ctx context.Context) {
	waiting, err := h.st.ListQueued(ctx)
	if err != nil {
		slog.Error("could not read the start queue", "err", err)
		return
	}
	for _, q := range waiting {
		h.promoteOne(ctx, q)
	}
}

func (h *Hub) promoteOne(ctx context.Context, q store.QueuedStart) {
	h.admission.RLock()
	defer h.admission.RUnlock()
	a, err := h.adapterFor(q.PeerAgent)
	if err != nil {
		// The agent is gone from this configuration; the entry would
		// otherwise wait forever.
		h.abandonQueued(ctx, q, "agent "+q.PeerAgent+" is not configured")
		return
	}
	if err := h.requireAvailable(ctx, q.PeerAgent); err != nil {
		return
	}
	if used, limit := a.Capacity(); limit > 0 && used >= limit {
		return
	}

	conv, err := h.st.GetConversation(ctx, q.ConversationID)
	if err != nil || conv.Status != store.StatusQueued {
		// Cancelled or already started while we were looking.
		_ = h.st.Dequeue(ctx, q.ConversationID)
		return
	}

	var r StartRequest
	if err := json.Unmarshal([]byte(q.RequestJSON), &r); err != nil {
		h.abandonQueued(ctx, q, "the queued request could not be decoded")
		return
	}
	// Dequeue first. A start that fails must not be retried forever;
	// the caller sees the failure through peer_poll and decides.
	if err := h.st.Dequeue(ctx, q.ConversationID); err != nil {
		slog.Error("could not dequeue", "conversation", q.ConversationID, "err", err)
		return
	}
	if _, err := h.startInto(ctx, q.ConversationID, r); err != nil {
		slog.Error("a queued start failed",
			"conversation", q.ConversationID, "agent", q.PeerAgent, "err", err)
		_ = h.st.SetConversationStatus(ctx, q.ConversationID, store.StatusStale)
		return
	}
	slog.Info("queued start promoted",
		"conversation", q.ConversationID, "agent", q.PeerAgent,
		"waited_s", int(time.Since(q.QueuedAt)/time.Second))
}

func (h *Hub) abandonQueued(ctx context.Context, q store.QueuedStart, reason string) {
	slog.Warn("dropping a queued start", "conversation", q.ConversationID, "reason", reason)
	_ = h.st.Dequeue(ctx, q.ConversationID)
	_ = h.st.SetConversationStatus(ctx, q.ConversationID, store.StatusStale)
}
