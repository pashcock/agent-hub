package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func TestInterruptEmitsInterruptedEvent(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"})
	ch, _ := a.Events(sid)

	if err := a.Interrupt(ctx, sid, "turn-1"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == adapter.EventInterrupted {
				return
			}
		case <-deadline:
			t.Fatal("no turn_interrupted event arrived")
		}
	}
}

func TestSteerIsAccepted(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"})
	if err := a.Steer(ctx, sid, "turn-1", "extra context"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
}

// provoke raises one server-to-client request through the fixture and
// returns the parked request plus the body the fixture echoed back.
func provoke(t *testing.T, a *Adapter, sid, method, decision string) (*adapter.ApprovalRequest, map[string]any) {
	t.Helper()
	ctx := context.Background()
	ch, _ := a.Events(sid)

	type echo struct {
		DecisionEcho map[string]any `json:"decisionEcho"`
	}
	done := make(chan echo, 1)
	errc := make(chan error, 1)
	go func() {
		var e echo
		if err := a.call(ctx, method, nil, &e); err != nil {
			errc <- err
			return
		}
		done <- e
	}()

	var req *adapter.ApprovalRequest
	deadline := time.After(5 * time.Second)
	for req == nil {
		select {
		case ev := <-ch:
			if ev.Kind == adapter.EventApproval {
				req = ev.Approval
			}
		case err := <-errc:
			t.Fatalf("%s failed before raising an approval: %v", method, err)
		case <-deadline:
			t.Fatalf("%s raised no approval_requested event", method)
		}
	}
	if req.ID == "" {
		t.Fatal("ApprovalRequest.ID must be set so Approve can address it")
	}
	if err := a.Approve(ctx, sid, req.ID, adapter.Decision{Verb: decision}); err != nil {
		t.Fatalf("Approve(%s): %v", decision, err)
	}
	select {
	case e := <-done:
		return req, e.DecisionEcho
	case err := <-errc:
		t.Fatalf("call failed after approval: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Approve did not unblock the server request")
	}
	return nil, nil
}

func TestCommandApprovalUsesADecisionVerb(t *testing.T) {
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(context.Background(), adapter.StartOptions{Cwd: "/tmp/p"})
	req, body := provoke(t, a, sid, "provoke/approval", "decline")

	if req.Class != adapter.ClassCommand {
		t.Errorf("Class = %q, want %q", req.Class, adapter.ClassCommand)
	}
	if req.Command != "sudo rm -rf /" {
		t.Errorf("Command = %q", req.Command)
	}
	if req.Cwd != "/tmp/p" {
		t.Errorf("Cwd = %q", req.Cwd)
	}
	if body["decision"] != "decline" {
		t.Errorf("response body = %v, want decision=decline", body)
	}
}

func TestPermissionsApprovalEchoesTheRequestedProfile(t *testing.T) {
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(context.Background(), adapter.StartOptions{Cwd: "/tmp/p"})

	// Accepting grants exactly what was asked for.
	req, body := provoke(t, a, sid, "provoke/permissions", "accept")
	if req.Class != adapter.ClassPermissions {
		t.Errorf("Class = %q, want %q", req.Class, adapter.ClassPermissions)
	}
	if body["scope"] != "turn" {
		t.Errorf("scope = %v, want turn", body["scope"])
	}
	granted, _ := json.Marshal(body["permissions"])
	if !strings.Contains(string(granted), "/tmp/p") {
		t.Errorf("granted profile lost the requested path: %s", granted)
	}

	// Declining grants an empty profile, never the requested one.
	_, body2 := provoke(t, a, sid, "provoke/permissions", "decline")
	granted2, _ := json.Marshal(body2["permissions"])
	if strings.Contains(string(granted2), "/tmp/p") {
		t.Errorf("declining must not grant anything, got %s", granted2)
	}
}

func TestUserInputApprovalAnswersEveryQuestion(t *testing.T) {
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(context.Background(), adapter.StartOptions{Cwd: "/tmp/p"})
	req, body := provoke(t, a, sid, "provoke/userinput", "decline")

	if req.Class != adapter.ClassUserInput {
		t.Errorf("Class = %q, want %q", req.Class, adapter.ClassUserInput)
	}
	answers, ok := body["answers"].(map[string]any)
	if !ok {
		t.Fatalf("response body = %v, want an answers object", body)
	}
	if _, ok := answers["q1"]; !ok {
		t.Errorf("every question id must be answered, got %v", answers)
	}
}

func TestElicitationApprovalUsesAnAction(t *testing.T) {
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(context.Background(), adapter.StartOptions{Cwd: "/tmp/p"})
	req, body := provoke(t, a, sid, "provoke/elicitation", "accept")

	if req.Class != adapter.ClassElicitation {
		t.Errorf("Class = %q, want %q", req.Class, adapter.ClassElicitation)
	}
	if body["action"] != "accept" {
		t.Errorf("response body = %v, want action=accept", body)
	}
}

func TestUnknownServerRequestIsDeclinedNotIgnored(t *testing.T) {
	ctx := context.Background()
	a := newFakeAdapter(t)
	_, _ = a.StartSession(ctx, adapter.StartOptions{Cwd: "/tmp/p"})

	// The fixture blocks until it gets a response. If an unrecognised
	// request were ignored, this call would hang until the deadline.
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var e struct {
		DecisionEcho map[string]any `json:"decisionEcho"`
	}
	if err := a.conn.Call(callCtx, "provoke/unknown", nil, &e); err != nil {
		t.Fatalf("an unrecognised request must still be answered: %v", err)
	}
	if e.DecisionEcho["decision"] != "decline" {
		t.Errorf("echo = %v, want decision=decline", e.DecisionEcho)
	}
}

func TestApproveRejectsUnknownRequestID(t *testing.T) {
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(context.Background(), adapter.StartOptions{Cwd: "/tmp/p"})
	if err := a.Approve(context.Background(), sid, "no-such-request", adapter.Decision{Verb: "accept"}); err == nil {
		t.Fatal("Approve must fail for an unknown request id")
	}
}

func TestApproveRejectsAnInvalidDecision(t *testing.T) {
	a := newFakeAdapter(t)
	sid, _ := a.StartSession(context.Background(), adapter.StartOptions{Cwd: "/tmp/p"})
	if err := a.Approve(context.Background(), sid, "anything", adapter.Decision{Verb: "maybe"}); err == nil {
		t.Fatal("Approve must reject a decision that is not in the protocol")
	}
}
