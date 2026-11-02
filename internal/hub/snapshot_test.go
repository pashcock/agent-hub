package hub

import (
	"context"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/store"
)

func TestSnapshotDoesNotProcessQueue(t *testing.T) {
	ctx := context.Background()
	snapshot, err := store.Open(t.TempDir() + "/snapshot.db")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	id, err := snapshot.CreateConversation(ctx, store.Conversation{PeerAgent: "grok", ProjectCwd: "/tmp/p", Status: store.StatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	if err = snapshot.SetConversationStatus(ctx, id, store.StatusQueued); err != nil {
		t.Fatal(err)
	}
	if err = snapshot.Enqueue(ctx, id, "grok", "{}"); err != nil {
		t.Fatal(err)
	}
	h := NewSnapshot(snapshot)
	defer h.Close()
	h.kick()
	time.Sleep(2 * queueTick)
	queued, err := snapshot.ListQueued(ctx)
	if err != nil || len(queued) != 1 {
		t.Fatalf("snapshot queue mutated: %v %v", queued, err)
	}
	c, err := snapshot.GetConversation(ctx, id)
	if err != nil || c.Status != store.StatusQueued {
		t.Fatalf("snapshot status mutated: %+v %v", c, err)
	}
}
