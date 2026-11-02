package mcpfacade

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

func TestOperatorCanAnswerAHumanGatedApproval(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fa := fake.New()
	h := hub.New(st, map[string]adapter.Adapter{"codex": fa}, hub.Config{
		MaxParallelPeerTurns: 3,
		HumanGated:           []string{"privilege_escalation"},
	})
	defer h.Close()
	f := New(h, []string{"codex"})

	ctx := t.Context()
	start, err := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "x", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	conv, _ := st.GetConversation(ctx, start.ConversationID)
	fa.Emit(conv.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: start.TurnID,
		Approval: &adapter.ApprovalRequest{
			ID: "ap_op", Class: adapter.ClassCommand,
			Command: "sudo systemctl restart nginx", Cwd: "/tmp/p"},
	})

	mux := http.NewServeMux()
	f.RegisterOperator(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Wait for the hub to absorb the approval event.
	deadline := time.After(5 * time.Second)
	for {
		res, err := http.Get(srv.URL + "/operator/approvals")
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Approvals []struct {
				ID         string `json:"id"`
				Command    string `json:"command"`
				HumanGated bool   `json:"human_gated"`
			} `json:"approvals"`
		}
		json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if len(body.Approvals) == 1 {
			if !body.Approvals[0].HumanGated {
				t.Fatal("a sudo command must be listed as human-gated")
			}
			if body.Approvals[0].Command != "sudo systemctl restart nginx" {
				t.Errorf("command = %q", body.Approvals[0].Command)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("approval never appeared: %+v", body)
		case <-time.After(20 * time.Millisecond):
		}
	}

	payload, _ := json.Marshal(map[string]string{
		"conversation_id": start.ConversationID,
		"request_id":      "ap_op",
		"decision":        "decline",
	})
	res, err := http.Post(srv.URL+"/operator/approve", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("operator approve returned %d", res.StatusCode)
	}

	if len(h.PendingApprovals()) != 0 {
		t.Error("an answered approval must be removed from the pending list")
	}
}

func TestOperatorApproveRejectsANonPostRequest(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	defer st.Close()
	h := hub.New(st, map[string]adapter.Adapter{}, hub.Config{})
	defer h.Close()
	mux := http.NewServeMux()
	New(h, nil).RegisterOperator(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/operator/approve")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}
