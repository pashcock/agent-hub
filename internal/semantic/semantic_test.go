package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/store"
)

type fixedScores []float64

func (s fixedScores) Scores(context.Context, string, []string) ([]float64, error) { return s, nil }
func TestRankLiteralFirstDeduplicateAndThreshold(t *testing.T) {
	docs := []store.SearchDocument{
		{ConversationID: "a", Text: "restore previous native session"},
		{ConversationID: "a", Text: "continued session"},
		{ConversationID: "b", Text: "chunk without full query", LiteralSnippet: "bug_42 literal"},
		{ConversationID: "c", Text: "unrelated"},
		{ConversationID: "d", Metadata: "/tmp/bug_42", Text: "metadata match"},
	}
	hits, err := Rank(context.Background(), fixedScores{.9, .8, .1, .2, .3}, "bug_42", docs)
	if err != nil || len(hits) != 3 || hits[0].ConversationID != "d" || hits[1].ConversationID != "b" || hits[2].ConversationID != "a" {
		t.Fatalf("%+v %v", hits, err)
	}
	if _, err := Rank(context.Background(), fixedScores{}, "q", docs); err == nil {
		t.Fatal("accepted missing scores")
	}
}

// Opt-in integration uses only existing local files; no downloads or providers.
func TestOfflineWorker(t *testing.T) {
	python, model := os.Getenv("HUB_TEST_SEMANTIC_PYTHON"), os.Getenv("HUB_TEST_SEMANTIC_MODEL")
	if python == "" || model == "" {
		t.Skip("local model paths not configured")
	}
	w, err := New(Config{python, model}, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	docs := []store.SearchDocument{{ConversationID: "a", Text: "Do not restart agent-hub while parallel agent tasks are running."}, {ConversationID: "b", Text: "A payment error charged a subscription twice."}}
	hits, err := Rank(ctx, w, "Keep the hub alive while other agents work", docs)
	if err != nil || len(hits) == 0 || hits[0].ConversationID != "a" {
		t.Fatalf("%+v %v", hits, err)
	}
	pid := w.cmd.Process.Pid
	// Changed text is encoded under a fresh hash; identical text reuses vectors.
	docs[0].Text = "Payment was charged twice for the same subscription."
	docs[1].Text = "Do not shut down the service while agents are still working."
	hits, err = Rank(ctx, w, "Keep the hub alive while other agents work", docs)
	if err != nil || len(hits) == 0 || hits[0].ConversationID != "b" {
		t.Fatalf("stale cache: %+v %v", hits, err)
	}
	if w.cmd.Process.Pid != pid {
		t.Fatal("worker respawned between searches")
	}
	w.stop()
	// A hung encoder must be killed on request timeout, without stranding pipe IO.
	hung := exec.Command(python, "-c", "import time; time.sleep(60)")
	input, err := hung.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := hung.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = hung.Start(); err != nil {
		t.Fatal(err)
	}
	w.cmd, w.input, w.output = hung, input, json.NewDecoder(output)
	short, cancelShort := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = w.Scores(short, "query", []string{"document"})
	cancelShort()
	if !errors.Is(err, context.DeadlineExceeded) || w.cmd != nil {
		t.Fatalf("hung worker retained: %v", err)
	}
	if _, err := Rank(ctx, w, "service restart", docs); err != nil {
		t.Fatal("restart failed:", err)
	}
}
