package hub

import (
	"context"
	"testing"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/store"
)

func parkGated(t *testing.T, h *Hub, fa interface {
	Emit(string, adapter.Event)
}, st *store.Store) (convID, approvalID string) {
	t.Helper()
	ctx := context.Background()
	res, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: res.TurnID,
		Approval: &adapter.ApprovalRequest{
			ID: "ap_gated", Class: adapter.ClassCommand,
			Command: "sudo systemctl restart nginx", Cwd: "/tmp/p"}})
	waitStatus(t, h, res.ConversationID, store.TurnNeedsPermission)
	return res.ConversationID, "ap_gated"
}

func TestPendingApprovalsCarryTheirConversation(t *testing.T) {
	h, fa, st := newHub(t)
	convID, _ := parkGated(t, h, fa, st)

	list := h.PendingApprovals()
	if len(list) != 1 {
		t.Fatalf("got %d pending approvals, want 1", len(list))
	}
	// Without this the operator cannot address the request at all: the
	// listing is the only place the id is ever shown.
	if list[0].ConversationID != convID {
		t.Errorf("ConversationID = %q, want %q", list[0].ConversationID, convID)
	}
}

func TestOperatorCanApproveWithoutNamingTheConversation(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	_, apID := parkGated(t, h, fa, st)

	// The hub already knows which conversation the request belongs to,
	// so the operator must not have to repeat it.
	if err := h.ApproveAsOperator(ctx, "", apID, adapter.Decision{Verb: "decline"}); err != nil {
		t.Fatalf("ApproveAsOperator without a conversation id: %v", err)
	}
	if len(h.PendingApprovals()) != 0 {
		t.Error("an answered approval must leave the pending list")
	}
}

func TestOperatorApproveStillRejectsAnUnknownRequest(t *testing.T) {
	h, _, _ := newHub(t)
	if err := h.ApproveAsOperator(context.Background(), "", "nope", adapter.Decision{Verb: "accept"}); err == nil {
		t.Fatal("an unknown approval id must be an error")
	}
}

func TestApproveRejectsAMismatchedConversationWhenOneIsGiven(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	_, apID := parkGated(t, h, fa, st)
	// Naming the wrong conversation is still a mistake worth catching.
	if err := h.ApproveAsOperator(ctx, "some-other-conversation", apID, adapter.Decision{Verb: "accept"}); err == nil {
		t.Fatal("a mismatched conversation id must be rejected")
	}
}
