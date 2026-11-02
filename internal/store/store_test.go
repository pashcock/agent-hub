package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetConversation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	c := Conversation{
		PeerAgent:  "codex",
		CallerKey:  "session-abc",
		ProjectCwd: "/tmp/proj",
		Model:      "gpt-5.6-luna",
		Effort:     "high",
		Mode:       "code",
	}
	id, err := s.CreateConversation(ctx, c)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if id == "" {
		t.Fatal("CreateConversation must return a non-empty id")
	}

	got, err := s.GetConversation(ctx, id)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Status != StatusLive {
		t.Errorf("Status = %q, want %q", got.Status, StatusLive)
	}
	if got.PeerAgent != "codex" || got.Model != "gpt-5.6-luna" || got.Effort != "high" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt must be set")
	}
}

func TestGetConversationUnknownID(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetConversation(context.Background(), "nope"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSetNativeSessionIDAndStatus(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	id, err := s.CreateConversation(ctx, Conversation{PeerAgent: "codex", ProjectCwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNativeSessionID(ctx, id, "thread-123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConversationStatus(ctx, id, StatusCold); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetConversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeSessionID != "thread-123" {
		t.Errorf("NativeSessionID = %q, want thread-123", got.NativeSessionID)
	}
	if got.Status != StatusCold {
		t.Errorf("Status = %q, want %q", got.Status, StatusCold)
	}
}

func TestSetConversationStatusRejectsUnknownState(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	id, _ := s.CreateConversation(ctx, Conversation{PeerAgent: "codex", ProjectCwd: "/tmp"})
	if err := s.SetConversationStatus(ctx, id, "banana"); err == nil {
		t.Fatal("an unknown liveness state must be rejected")
	}
}

func TestMarkAllColdLeavesClosedAlone(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	liveID, _ := s.CreateConversation(ctx, Conversation{PeerAgent: "codex", ProjectCwd: "/tmp"})
	closedID, _ := s.CreateConversation(ctx, Conversation{PeerAgent: "codex", ProjectCwd: "/tmp"})
	if err := s.SetConversationStatus(ctx, closedID, StatusClosed); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkAllCold(ctx); err != nil {
		t.Fatal(err)
	}

	live, _ := s.GetConversation(ctx, liveID)
	if live.Status != StatusCold {
		t.Errorf("live conversation Status = %q, want %q", live.Status, StatusCold)
	}
	closed, _ := s.GetConversation(ctx, closedID)
	if closed.Status != StatusClosed {
		t.Errorf("closed conversation Status = %q, want %q", closed.Status, StatusClosed)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hub.db")
	s1, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := s1.CreateConversation(context.Background(), Conversation{PeerAgent: "codex", ProjectCwd: "/tmp"})
	s1.Close()

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("reopening an existing database must succeed: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetConversation(context.Background(), id); err != nil {
		t.Fatalf("data must survive reopen: %v", err)
	}
}
