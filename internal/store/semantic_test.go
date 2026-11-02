package store

import (
	"context"
	"strings"
	"testing"
)

func TestSemanticDocumentsScopeChunksAndFreshness(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	id, err := s.CreateConversation(ctx, Conversation{PeerAgent: "grok", ProjectCwd: "/tmp/a", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateConversation(ctx, Conversation{PeerAgent: "grok", ProjectCwd: "/tmp/a-worktree"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AppendMessage(ctx, id, "", "user", strings.Repeat("界", 1190)+"boundary match"+strings.Repeat("a", 1000)); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendMessage(ctx, other, "", "user", "MUST_NOT_LEAK"); err != nil {
		t.Fatal(err)
	}
	f := ConversationFilter{Cwd: "/tmp/a", Query: "not a literal match"}
	docs, err := s.SearchDocuments(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range docs {
		if d.ConversationID != id || strings.Contains(d.Text, "MUST_NOT_LEAK") {
			t.Fatal("project leakage")
		}
		if strings.Contains(d.Text, "boundary match") {
			found = true
		}
		if len([]rune(d.Text)) > 1200 {
			t.Fatal("unbounded chunk")
		}
	}
	if !found {
		t.Fatal("chunk boundary lost text")
	}
	f.Query = strings.Repeat("界", 300) + "boundary match"
	matched, err := s.SearchDocuments(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) == 0 || !strings.Contains(matched[0].LiteralSnippet, f.Query) {
		t.Fatal("long literal crossing chunks lost")
	}
	f.Query = "not a literal match"
	if err = s.AppendMessage(ctx, id, "", "assistant", "newly added"); err != nil {
		t.Fatal(err)
	}
	docs, err = s.SearchDocuments(ctx, f)
	if err != nil || docs[len(docs)-1].Text != "newly added" {
		t.Fatalf("stale documents: %v", err)
	}
	rows, n, err := s.SearchConversations(ctx, ConversationFilter{IDs: []string{other, id}, Limit: 1, Offset: 1})
	if err != nil || n != 2 || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("rank pagination: %v %d %v", rows, n, err)
	}
	_, n, err = s.SearchConversations(ctx, ConversationFilter{IDs: []string{}})
	if err != nil || n != 0 {
		t.Fatalf("empty matches exposed all: %d %v", n, err)
	}
}

func TestRepeatedUsageReplacesRatherThanAdds(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	id := seedConversation(t, s)
	turn, err := s.CreateTurn(ctx, Turn{ConversationID: id})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.RecordUsage(ctx, id, turn, Usage{TotalTokens: 105}); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := s.ConversationUsage(ctx, id)
	if err != nil || usage.TotalTokens != 105 {
		t.Fatalf("duplicate usage: %+v %v", usage, err)
	}
}

func TestReadOnlySnapshotRejectsMutations(t *testing.T) {
	path := t.TempDir() + "/snapshot.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := seedConversation(t, s)
	s.Close()
	s, err = OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.GetConversation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err = s.SetConversationStatus(context.Background(), id, StatusClosed); err == nil {
		t.Fatal("read-only snapshot accepted write")
	}
}
