package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

func TestSearchAndListAPIShareFilters(t *testing.T) {
	srv, h, _, _ := harness(t)
	ctx := context.Background()
	a, err := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "repair mango", Cwd: "/tmp/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "repair pear", Cwd: "/tmp/alpha-worktree"})
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/?cwd="+url.QueryEscape("/tmp/alpha")+"&q=mango")
	if !strings.Contains(body, "/c/"+a.ConversationID) || strings.Contains(body, "/c/"+b.ConversationID) {
		t.Fatal("workspace search mixed conversations")
	}
	code, out := getJSON(t, srv, "/api/conversations?cwd="+url.QueryEscape("/tmp/alpha")+"&q=mango")
	if code != 200 || out["total"] != float64(1) {
		t.Fatalf("list API: %v", out)
	}
}
func TestConversationShowsActualLatestEvents(t *testing.T) {
	srv, _, _, st := harness(t)
	ctx := context.Background()
	id, err := st.CreateConversation(ctx, store.Conversation{PeerAgent: "codex", ProjectCwd: "/tmp/demo"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 650; i++ {
		text := `{"old":true}`
		if i == 649 {
			text = `{"TAIL_MARKER":true}`
		}
		if err := st.AppendEvent(ctx, id, "", "raw", json.RawMessage(text)); err != nil {
			t.Fatal(err)
		}
	}
	body := get(t, srv, "/c/"+id)
	if !strings.Contains(body, "TAIL_MARKER") || !strings.Contains(body, "Older events") {
		t.Fatal("latest event or pagination missing")
	}
}
func TestProviderFormPausesAndResumes(t *testing.T) {
	srv, h, _, _ := harness(t)
	for _, action := range []string{"pause", "resume"} {
		res, err := http.PostForm(srv.URL+"/act/provider", url.Values{"agent": {"codex"}, "action": {action}, "reason": {"quota"}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("action %s: %d", action, res.StatusCode)
		}
		if h.AgentStatuses()[0].Availability.Paused != (action == "pause") {
			t.Fatal("wrong provider state")
		}
	}
}
