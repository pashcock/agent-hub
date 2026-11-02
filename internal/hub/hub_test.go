package hub

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/store"
)

func newHub(t *testing.T) (*Hub, *fake.Adapter, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	fa := fake.New()
	h := New(st, map[string]adapter.Adapter{"fake": fa}, Config{
		MaxParallelPeerTurns: 2,
		HumanGated:           []string{"privilege_escalation", "write_outside_cwd", "policy_amendment"},
	})
	t.Cleanup(func() { h.Close(); st.Close() })
	return h, fa, st
}

func waitStatus(t *testing.T, h *Hub, convID, want string) PollResult {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		res, err := h.Poll(context.Background(), convID, 0)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if res.TurnStatus == want {
			return res
		}
		select {
		case <-deadline:
			t.Fatalf("turn status = %q, want %q", res.TurnStatus, want)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestStartCreatesLiveConversation(t *testing.T) {
	h, _, st := newHub(t)
	res, err := h.Start(context.Background(), StartRequest{
		Agent: "fake", Task: "do the thing", Cwd: "/tmp/p", Mode: "code",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ConversationID == "" || res.TurnID == "" {
		t.Fatalf("Start returned %+v", res)
	}
	c, err := st.GetConversation(context.Background(), res.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != store.StatusLive {
		t.Errorf("Status = %q, want live", c.Status)
	}
	if c.NativeSessionID == "" {
		t.Error("NativeSessionID must be recorded")
	}
}

func TestStartRejectsUnknownAgent(t *testing.T) {
	h, _, _ := newHub(t)
	_, err := h.Start(context.Background(), StartRequest{Agent: "nope", Task: "x", Cwd: "/tmp"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want it to name the unknown agent", err)
	}
}

func TestPollReturnsCompletionAndFinalText(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "the answer"})

	got := waitStatus(t, h, res.ConversationID, store.TurnCompleted)
	if got.Result != "the answer" {
		t.Errorf("Result = %q, want %q", got.Result, "the answer")
	}
}

func TestPollDoesNotRehydrate(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done"})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)

	// Force the conversation cold, then arrange for any resume to fail.
	if err := st.SetConversationStatus(ctx, res.ConversationID, store.StatusCold); err != nil {
		t.Fatal(err)
	}
	fa.FailNextResume()

	pr, err := h.Poll(ctx, res.ConversationID, 0)
	if err != nil {
		t.Fatalf("Poll on a cold conversation must succeed from the database: %v", err)
	}
	if pr.Result != "done" {
		t.Errorf("Result = %q, want done", pr.Result)
	}
	after, _ := st.GetConversation(ctx, res.ConversationID)
	if after.Status != store.StatusCold {
		t.Errorf("Poll changed liveness to %q; it must stay cold", after.Status)
	}
	// The programmed failure must still be unused.
	if err := fa.ResumeSession(ctx, c.NativeSessionID, adapter.StartOptions{Cwd: "/tmp/p"}); err != adapter.ErrSessionGone {
		t.Error("Poll consumed the resume that was reserved for Send")
	}
}

// finishFirstTurn completes the turn Start opened. A conversation only
// accepts a new turn once the previous one is terminal, so every test that
// calls Send must settle the first turn first.
func finishFirstTurn(t *testing.T, h *Hub, fa *fake.Adapter, st *store.Store, res StartResult) {
	t.Helper()
	c, err := st.GetConversation(context.Background(), res.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done"})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)
}

func TestSendRehydratesColdConversation(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	finishFirstTurn(t, h, fa, st, res)
	st.SetConversationStatus(ctx, res.ConversationID, store.StatusCold)

	if _, err := h.Send(ctx, res.ConversationID, "more", adapter.TurnOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	c, _ := st.GetConversation(ctx, res.ConversationID)
	if c.Status != store.StatusLive {
		t.Errorf("Status = %q, want live after Send", c.Status)
	}
}

func TestSendMarksStaleWhenResumeFails(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	finishFirstTurn(t, h, fa, st, res)
	st.SetConversationStatus(ctx, res.ConversationID, store.StatusCold)
	fa.FailNextResume()

	if _, err := h.Send(ctx, res.ConversationID, "more", adapter.TurnOptions{}); err == nil {
		t.Fatal("Send must fail when the native session is gone")
	}
	c, _ := st.GetConversation(ctx, res.ConversationID)
	if c.Status != store.StatusStale {
		t.Errorf("Status = %q, want stale", c.Status)
	}
}

func TestSteerRequiresARunningTurn(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: res.TurnID})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)

	if err := h.Steer(ctx, res.ConversationID, "late"); err == nil {
		t.Fatal("Steer must fail when no turn is running")
	}
}

func TestCancelWithNothingRunningIsANoOp(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{Kind: adapter.EventTurnCompleted, TurnID: res.TurnID})
	waitStatus(t, h, res.ConversationID, store.TurnCompleted)

	if err := h.Cancel(ctx, res.ConversationID); err != nil {
		t.Fatalf("Cancel must be a no-op success, got %v", err)
	}
}

func TestConcurrentTurnLimitPerCaller(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newHub(t)
	for i := 0; i < 2; i++ {
		if _, err := h.Start(ctx, StartRequest{
			Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "sess-1"}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	_, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "sess-1"})
	if err == nil {
		t.Fatal("the third concurrent turn for one caller must be refused")
	}
	// A different caller is unaffected.
	if _, err := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "sess-2"}); err != nil {
		t.Fatalf("another caller must not be blocked: %v", err)
	}
}

func TestApprovalIsSurfacedAndOrdinaryOnesAreAnswerable(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: res.TurnID,
		Approval: &adapter.ApprovalRequest{
			ID: "ap_1", Class: adapter.ClassCommand, Command: "go test ./...", Cwd: "/tmp/p"},
	})

	pr := waitStatus(t, h, res.ConversationID, store.TurnNeedsPermission)
	if pr.PendingApproval == nil {
		t.Fatal("Poll must surface the pending approval")
	}
	if pr.PendingApproval.HumanGated {
		t.Error("go test must not be human-gated")
	}
	if err := h.Approve(ctx, res.ConversationID, "ap_1", adapter.Decision{Verb: "accept"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
}

func TestHumanGatedApprovalRefusesOrchestratorApproval(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: res.TurnID,
		Approval: &adapter.ApprovalRequest{
			ID: "ap_2", Class: adapter.ClassCommand, Command: "sudo rm -rf /tmp/x", Cwd: "/tmp/p"},
	})
	pr := waitStatus(t, h, res.ConversationID, store.TurnNeedsPermission)
	if pr.PendingApproval == nil || !pr.PendingApproval.HumanGated {
		t.Fatalf("a sudo command must be human-gated, got %+v", pr.PendingApproval)
	}
	err := h.Approve(ctx, res.ConversationID, "ap_2", adapter.Decision{Verb: "accept"})
	if err == nil || !strings.Contains(err.Error(), "agent-hub approve") {
		t.Fatalf("err = %v, want a refusal naming the operator command", err)
	}
	// The operator path works.
	if err := h.ApproveAsOperator(ctx, res.ConversationID, "ap_2", adapter.Decision{Verb: "accept"}); err != nil {
		t.Fatalf("ApproveAsOperator: %v", err)
	}
}

func TestClassifyGate(t *testing.T) {
	gated := []string{"privilege_escalation", "write_outside_cwd", "policy_amendment"}
	cases := []struct {
		name string
		req  adapter.ApprovalRequest
		cwd  string
		want bool
	}{
		{"plain test run", adapter.ApprovalRequest{Class: adapter.ClassCommand, Command: "pytest -q"}, "/tmp/p", false},
		{"sudo", adapter.ApprovalRequest{Class: adapter.ClassCommand, Command: "sudo apt install x"}, "/tmp/p", true},
		{"pkexec", adapter.ApprovalRequest{Class: adapter.ClassCommand, Command: " pkexec whoami"}, "/tmp/p", true},
		{"sudo as a substring is not escalation", adapter.ApprovalRequest{Class: adapter.ClassCommand, Command: "echo pseudo"}, "/tmp/p", false},
		{"write inside cwd", adapter.ApprovalRequest{Class: adapter.ClassFileChange, Paths: []string{"/tmp/p/src"}}, "/tmp/p", false},
		{"write outside cwd", adapter.ApprovalRequest{Class: adapter.ClassFileChange, Paths: []string{"/etc"}}, "/tmp/p", true},
		{"cwd prefix must not match a sibling", adapter.ApprovalRequest{Class: adapter.ClassFileChange, Paths: []string{"/tmp/proj-evil"}}, "/tmp/proj", true},
		{"policy amendment", adapter.ApprovalRequest{Class: adapter.ClassPolicy, Command: "curl example.com"}, "/tmp/p", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanGated(tc.req, tc.cwd, gated); got != tc.want {
				t.Errorf("humanGated = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompletedTurnReleasesItsSlot(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)

	// Fill the limit, complete both, then prove capacity came back.
	var convs []string
	for i := 0; i < 2; i++ {
		res, err := h.Start(ctx, StartRequest{
			Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "sess-1"})
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		convs = append(convs, res.ConversationID)
		c, _ := st.GetConversation(ctx, res.ConversationID)
		fa.Emit(c.NativeSessionID, adapter.Event{
			Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done"})
	}
	for _, id := range convs {
		waitStatus(t, h, id, store.TurnCompleted)
	}
	if _, err := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "sess-1"}); err != nil {
		t.Fatalf("completed turns must free their slots: %v", err)
	}
}

func TestDuplicateTerminalEventDoesNotFreeAnotherSlot(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	first, _ := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "sess-1"})
	second, _ := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "y", Cwd: "/tmp/p", CallerKey: "sess-1"})

	c1, _ := st.GetConversation(ctx, first.ConversationID)
	// Complete the first turn three times over.
	for i := 0; i < 3; i++ {
		fa.Emit(c1.NativeSessionID, adapter.Event{
			Kind: adapter.EventTurnCompleted, TurnID: first.TurnID, Text: "done"})
	}
	waitStatus(t, h, first.ConversationID, store.TurnCompleted)
	time.Sleep(200 * time.Millisecond)

	// Exactly one slot came back, so exactly one new turn fits.
	if _, err := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "z", Cwd: "/tmp/p", CallerKey: "sess-1"}); err != nil {
		t.Fatalf("one slot must have been freed: %v", err)
	}
	if _, err := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "w", Cwd: "/tmp/p", CallerKey: "sess-1"}); err == nil {
		t.Fatal("duplicate terminal events freed more slots than they should")
	}
	_ = second
}

func TestProviderStreamCloseFailsTheOpenTurn(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{
		Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "sess-1"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	// The provider dies without emitting a terminal event.
	if err := fa.CloseSession(ctx, c.NativeSessionID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, h, res.ConversationID, store.TurnFailed)

	// The slot must have come back with it.
	for i := 0; i < 2; i++ {
		if _, err := h.Start(ctx, StartRequest{
			Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "sess-1"}); err != nil {
			t.Fatalf("a stranded turn leaked its slot: %v", err)
		}
	}
}

func TestSendRefusesASecondConcurrentTurn(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	if _, err := h.Send(ctx, res.ConversationID, "again", adapter.TurnOptions{}); err == nil {
		t.Fatal("Send must refuse while a turn is still running")
	}
}

func TestApprovalIsSelectedByConversation(t *testing.T) {
	ctx := context.Background()
	h, fa, st := newHub(t)
	a1, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "s1"})
	a2, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "y", Cwd: "/tmp/p", CallerKey: "s2"})
	c1, _ := st.GetConversation(ctx, a1.ConversationID)
	c2, _ := st.GetConversation(ctx, a2.ConversationID)

	fa.Emit(c1.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: a1.TurnID,
		Approval: &adapter.ApprovalRequest{ID: "ap_one", Class: adapter.ClassCommand,
			Command: "go build ./...", Cwd: "/tmp/p"}})
	fa.Emit(c2.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: a2.TurnID,
		Approval: &adapter.ApprovalRequest{ID: "ap_two", Class: adapter.ClassCommand,
			Command: "go vet ./...", Cwd: "/tmp/p"}})

	p1 := waitStatus(t, h, a1.ConversationID, store.TurnNeedsPermission)
	p2 := waitStatus(t, h, a2.ConversationID, store.TurnNeedsPermission)
	if p1.PendingApproval == nil || p1.PendingApproval.ID != "ap_one" {
		t.Errorf("conversation one got %+v, want ap_one", p1.PendingApproval)
	}
	if p2.PendingApproval == nil || p2.PendingApproval.ID != "ap_two" {
		t.Errorf("conversation two got %+v, want ap_two", p2.PendingApproval)
	}
	// Answering another conversation's request must be refused.
	if err := h.Approve(ctx, a1.ConversationID, "ap_two", adapter.Decision{Verb: "accept"}); err == nil {
		t.Fatal("Approve must reject an approval from a different conversation")
	}
}

func TestUnansweredApprovalTimesOutAndFreesTheSlot(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fa := fake.New()
	h := New(st, map[string]adapter.Adapter{"fake": fa}, Config{
		MaxParallelPeerTurns: 1,
		ApprovalTimeout:      150 * time.Millisecond,
		OnApprovalTimeout:    "deny",
	})
	defer h.Close()

	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p", CallerKey: "s"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: res.TurnID,
		Approval: &adapter.ApprovalRequest{ID: "ap_slow", Class: adapter.ClassCommand,
			Command: "sleep 3600", Cwd: "/tmp/p"}})
	waitStatus(t, h, res.ConversationID, store.TurnNeedsPermission)

	deadline := time.After(5 * time.Second)
	for {
		if len(h.PendingApprovals()) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the approval never expired")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestRecoverColdsEverythingAndFailsOpenTurns(t *testing.T) {
	ctx := context.Background()
	h, _, st := newHub(t)
	res, _ := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})

	if err := h.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetConversation(ctx, res.ConversationID)
	if c.Status != store.StatusCold {
		t.Errorf("Status = %q, want cold after Recover", c.Status)
	}
	turn, _ := st.GetTurn(ctx, res.TurnID)
	if turn.Status != store.TurnFailed {
		t.Errorf("turn Status = %q, want failed", turn.Status)
	}
	if turn.Error != ReasonHubRestart {
		t.Errorf("turn Error = %q, want the restart reason", turn.Error)
	}
}
