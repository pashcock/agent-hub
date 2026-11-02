package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/store"
)

type closeFailureAdapter struct {
	adapter.Adapter
	err error
}

func (a *closeFailureAdapter) CloseSession(ctx context.Context, id string) error {
	if a.err != nil {
		return a.err
	}
	return a.Adapter.CloseSession(ctx, id)
}

func TestCloseFailureDoesNotReportClosedAndCanRetry(t *testing.T) {
	h, fa, st := newHub(t)
	wrapped := &closeFailureAdapter{Adapter: fa, err: errors.New("unsubscribe unavailable")}
	h.adapters["fake"] = wrapped
	ctx := context.Background()
	res, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "work", Cwd: "/tmp/p", Mode: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.CloseConversation(ctx, res.ConversationID); err == nil || !strings.Contains(err.Error(), "unsubscribe unavailable") {
		t.Fatalf("close error: %v", err)
	}
	conv, err := st.GetConversation(ctx, res.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Status == store.StatusClosed {
		t.Fatal("failed cleanup falsely marked closed")
	}
	turn, err := st.LatestTurn(ctx, res.ConversationID)
	if err != nil || turn.Status != store.TurnRunning {
		t.Fatalf("failed close released running turn: %+v, %v", turn, err)
	}
	wrapped.err = nil
	if err = h.CloseConversation(ctx, res.ConversationID); err != nil {
		t.Fatal(err)
	}
	conv, err = st.GetConversation(ctx, res.ConversationID)
	if err != nil || conv.Status != store.StatusClosed {
		t.Fatalf("retry: %+v, %v", conv, err)
	}
}

// Block native cleanup so a concurrent send hits the window between process
// removal and storing the closed conversation status.
type blockingCloseAdapter struct {
	adapter.Adapter
	entered chan struct{}
	release chan struct{}
}

func (a *blockingCloseAdapter) CloseSession(ctx context.Context, id string) error {
	close(a.entered)
	<-a.release
	return a.Adapter.CloseSession(ctx, id)
}
func TestCloseSerializesConcurrentSend(t *testing.T) {
	h, fa, st := newHub(t)
	wrapped := &blockingCloseAdapter{Adapter: fa, entered: make(chan struct{}), release: make(chan struct{})}
	h.adapters["fake"] = wrapped
	ctx := context.Background()
	res, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "work", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: res.TurnID})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)
	closed := make(chan error, 1)
	go func() { closed <- h.CloseConversation(ctx, res.ConversationID) }()
	<-wrapped.entered
	sent := make(chan error, 1)
	go func() { _, err := h.Send(ctx, res.ConversationID, "next", adapter.TurnOptions{}); sent <- err }()
	select {
	case err := <-sent:
		close(wrapped.release)
		<-closed
		t.Fatalf("send escaped pending close: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(wrapped.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("send after close: %v", err)
	}
}
