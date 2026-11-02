package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pashcock/agent-hub/internal/store"
)

type recordingScorer struct {
	fail  bool
	texts []string
}

func (s *recordingScorer) Scores(_ context.Context, _ string, texts []string) ([]float64, error) {
	s.texts = texts
	if s.fail {
		return nil, errors.New("offline worker stopped")
	}
	scores := make([]float64, len(texts))
	for i := range scores {
		scores[i] = .8
	}
	return scores, nil
}
func TestHybridUIAndAPIProjectScopeAndFallback(t *testing.T) {
	_, h, _, st := harness(t)
	ctx := context.Background()
	a, err := st.CreateConversation(ctx, store.Conversation{PeerAgent: "grok", ProjectCwd: "/tmp/a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateConversation(ctx, store.Conversation{PeerAgent: "grok", ProjectCwd: "/tmp/b"})
	if err != nil {
		t.Fatal(err)
	}
	st.AppendMessage(ctx, a, "", "user", "restore previous native session")
	st.AppendMessage(ctx, b, "", "user", "PRIVATE_OTHER_PROJECT")
	scorer := &recordingScorer{}
	ui, err := New(h, st)
	if err != nil {
		t.Fatal(err)
	}
	ui.Semantic = scorer
	mux := http.NewServeMux()
	ui.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body := get(t, srv, "/?search=hybrid&cwd=/tmp/a&q=resume")
	if !strings.Contains(body, "/c/"+a) || strings.Contains(body, "/c/"+b) || !strings.Contains(body, "meaning match") {
		t.Fatal("hybrid UI wrong")
	}
	if strings.Contains(strings.Join(scorer.texts, " "), "PRIVATE_OTHER_PROJECT") {
		t.Fatal("encoder saw other project")
	}
	code, out := getJSON(t, srv, "/api/conversations?search=hybrid&cwd=/tmp/a&q=resume")
	if code != 200 || out["total"] != float64(1) {
		t.Fatalf("%v", out)
	}
	scorer.fail = true
	code, out = getJSON(t, srv, "/api/conversations?search=hybrid&cwd=/tmp/a&q=restore")
	if code != 200 || out["total"] != float64(1) || !strings.Contains(out["notice"].(string), "literal matches") {
		t.Fatalf("fallback: %v", out)
	}
	body = get(t, srv, "/?search=literal&cwd=/tmp/a&q=resume")
	if strings.Contains(body, "/c/"+a) {
		t.Fatal("literal mode used semantic scores")
	}
}
