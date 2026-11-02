package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

func harness(t *testing.T) (*httptest.Server, *hub.Hub, *fake.Adapter, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	fa := fake.New()
	h := hub.New(st, map[string]adapter.Adapter{"codex": fa},
		hub.Config{MaxParallelPeerTurns: 5})
	ui, err := New(h, st)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	ui.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); h.Close(); st.Close() })
	return srv, h, fa, st
}

func get(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, res.StatusCode)
	}
	b := make([]byte, 1<<20)
	n, _ := res.Body.Read(b)
	body := string(b[:n])
	for {
		m, err := res.Body.Read(b)
		if m > 0 {
			body += string(b[:m])
		}
		if err != nil {
			break
		}
	}
	return body
}

func TestOverviewShowsAgentsAndConversations(t *testing.T) {
	srv, h, _, _ := harness(t)
	res, err := h.Start(context.Background(), hub.StartRequest{
		Agent: "codex", Task: "hello there", Cwd: "/tmp/demo"})
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/")
	for _, want := range []string{"codex", "/c/" + res.ConversationID, "/tmp/demo", "running"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview is missing %q", want)
		}
	}
}

func TestConversationShowsUsageAndDialogue(t *testing.T) {
	ctx := context.Background()
	srv, h, fa, st := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "count it", Cwd: "/tmp/demo"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "the answer",
		Usage: &adapter.Usage{Model: "gpt-5.6-luna", InputTokens: 1234,
			OutputTokens: 56, TotalTokens: 1290, CostUSD: 0.25}})

	deadline := time.After(5 * time.Second)
	for {
		if u, _ := st.TurnUsage(ctx, res.TurnID); u.TotalTokens == 1290 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("usage never landed")
		case <-time.After(20 * time.Millisecond):
		}
	}

	body := get(t, srv, "/c/"+res.ConversationID)
	for _, want := range []string{"count it", "the answer", "gpt-5.6-luna", "1 290", "$0.2500"} {
		if !strings.Contains(body, want) {
			t.Errorf("conversation page is missing %q", want)
		}
	}
}

func TestConversationPageOffersAWayBack(t *testing.T) {
	ctx := context.Background()
	srv, h, _, _ := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "look around", Cwd: "/tmp/demo"})

	// Without a control on the page the only way out is the browser's
	// own back button, which the transcript's auto-refresh fights.
	body := get(t, srv, "/c/"+res.ConversationID)
	if n := strings.Count(body, `class="back" href="/"`); n < 2 {
		t.Errorf("back controls on the conversation page = %d, want one at the top and one at the bottom", n)
	}
}

func TestCancelButtonStopsTheTurn(t *testing.T) {
	ctx := context.Background()
	srv, h, _, _ := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "x", Cwd: "/tmp/demo"})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	form := url.Values{"conversation": {res.ConversationID}, "back": {"/"}}
	r, err := client.PostForm(srv.URL+"/act/cancel", form)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("cancel returned %d, want a redirect", r.StatusCode)
	}
}

func TestApproveButtonAnswersAHumanGatedRequest(t *testing.T) {
	ctx := context.Background()
	srv, h, fa, st := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "x", Cwd: "/tmp/demo"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventApproval, TurnID: res.TurnID,
		Approval: &adapter.ApprovalRequest{ID: "ap_web", Class: adapter.ClassCommand,
			Command: "sudo reboot", Cwd: "/tmp/demo"}})
	deadline := time.After(5 * time.Second)
	for len(h.PendingApprovals()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the approval never parked")
		case <-time.After(20 * time.Millisecond):
		}
	}
	// The web view must be able to answer what peer_approve refuses.
	if !strings.Contains(get(t, srv, "/"), "sudo reboot") {
		t.Error("the overview does not show the waiting request")
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	r, err := client.PostForm(srv.URL+"/act/approve",
		url.Values{"request": {"ap_web"}, "decision": {"decline"}, "back": {"/"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve returned %d", r.StatusCode)
	}
	if len(h.PendingApprovals()) != 0 {
		t.Error("the request is still pending after being declined")
	}
}

func TestActionsRefuseGet(t *testing.T) {
	srv, _, _, _ := harness(t)
	for _, p := range []string{"/act/cancel", "/act/approve"} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", p, res.StatusCode)
		}
	}
}
