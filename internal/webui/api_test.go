package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/hub"
)

func getJSON(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s: body is not JSON: %v", path, err)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s: content type = %q", path, ct)
	}
	return res.StatusCode, out
}

func TestStatusReportsARunningTurn(t *testing.T) {
	ctx := context.Background()
	srv, h, _, _ := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "think", Cwd: "/tmp/demo"})

	code, got := getJSON(t, srv, "/api/c/"+res.ConversationID)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got["turn_status"] != "running" {
		t.Errorf("turn_status = %v, want running", got["turn_status"])
	}
	// done is the field a waiting loop tests. It has to be there while
	// the turn is still going, and it has to be false.
	if got["done"] != false {
		t.Errorf("done = %v, want false", got["done"])
	}
	if got["conversation_id"] != res.ConversationID {
		t.Errorf("conversation_id = %v", got["conversation_id"])
	}
	if got["agent"] != "codex" {
		t.Errorf("agent = %v, want codex", got["agent"])
	}
}

func TestStatusReportsTheAnswerAndWhatItCost(t *testing.T) {
	ctx := context.Background()
	srv, h, fa, st := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "count", Cwd: "/tmp/demo"})
	c, _ := st.GetConversation(ctx, res.ConversationID)
	fa.Emit(c.NativeSessionID, adapter.Event{
		Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "the answer",
		Usage: &adapter.Usage{Model: "gpt-6-astra", TotalTokens: 1290, CostUSD: 0.25}})

	deadline := time.After(5 * time.Second)
	for {
		_, got := getJSON(t, srv, "/api/c/"+res.ConversationID)
		if got["done"] == true {
			if got["turn_status"] != "completed" {
				t.Errorf("turn_status = %v, want completed", got["turn_status"])
			}
			if got["result"] != "the answer" {
				t.Errorf("result = %v", got["result"])
			}
			u, _ := got["usage"].(map[string]any)
			if u == nil || u["total_tokens"] != float64(1290) {
				t.Errorf("usage = %v, want the finished turn's tokens", got["usage"])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("the turn never reported done")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestStatusOfAnUnknownConversationIs404JSON(t *testing.T) {
	srv, _, _, _ := harness(t)
	code, got := getJSON(t, srv, "/api/c/nope")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
	// Even the error is JSON: a waiting loop parses one shape, always.
	if got["error"] == nil || got["error"] == "" {
		t.Errorf("404 body carries no error: %v", got)
	}
}

func TestWaitReturnsAsSoonAsTheTurnEnds(t *testing.T) {
	ctx := context.Background()
	srv, h, fa, st := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "count", Cwd: "/tmp/demo"})
	c, _ := st.GetConversation(ctx, res.ConversationID)

	go func() {
		time.Sleep(300 * time.Millisecond)
		fa.Emit(c.NativeSessionID, adapter.Event{
			Kind: adapter.EventTurnCompleted, TurnID: res.TurnID, Text: "done thinking"})
	}()

	started := time.Now()
	code, got := getJSON(t, srv, "/api/c/"+res.ConversationID+"/wait?timeout_s=30")
	took := time.Since(started)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got["done"] != true || got["result"] != "done thinking" {
		t.Fatalf("wait returned %v", got)
	}
	if got["timed_out"] != false {
		t.Errorf("timed_out = %v, want false", got["timed_out"])
	}
	// The point of the endpoint: it comes back when the peer is done,
	// not when a timer says so.
	if took > 10*time.Second {
		t.Errorf("wait took %s, so it was polling on a timer, not waiting", took)
	}
}

func TestWaitGivesUpWithTheCurrentStateRatherThanAnError(t *testing.T) {
	ctx := context.Background()
	srv, h, _, _ := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "think", Cwd: "/tmp/demo"})

	code, got := getJSON(t, srv, "/api/c/"+res.ConversationID+"/wait?timeout_s=1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want the state anyway", code)
	}
	if got["timed_out"] != true {
		t.Errorf("timed_out = %v, want true", got["timed_out"])
	}
	if got["done"] != false || got["turn_status"] != "running" {
		t.Errorf("a timed-out wait must still describe the turn: %v", got)
	}
}

func TestWaitCapsTheTimeoutItWasAsked(t *testing.T) {
	if maxWaitS < 60 {
		t.Fatalf("maxWaitS = %d, too short to be useful", maxWaitS)
	}
	for _, tc := range []struct{ asked, want int }{
		{0, defaultWaitS},
		{-5, defaultWaitS},
		{30, 30},
		{maxWaitS + 600, maxWaitS},
	} {
		if got := waitBudget(tc.asked); got != tc.want {
			t.Errorf("waitBudget(%d) = %d, want %d", tc.asked, got, tc.want)
		}
	}
}

func TestAPIRefusesWrites(t *testing.T) {
	ctx := context.Background()
	srv, h, _, _ := harness(t)
	res, _ := h.Start(ctx, hub.StartRequest{Agent: "codex", Task: "think", Cwd: "/tmp/demo"})

	for _, path := range []string{"/api/c/" + res.ConversationID, "/api/c/" + res.ConversationID + "/wait"} {
		r, err := http.Post(srv.URL+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, r.StatusCode)
		}
	}
}
