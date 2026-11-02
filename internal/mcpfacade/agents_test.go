package mcpfacade

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

type catalogAdapter struct {
	*fake.Adapter
	cwds chan string
}

func (a *catalogAdapter) Capabilities(ctx context.Context, cwd string) (adapter.Capabilities, error) {
	a.cwds <- cwd
	return a.Adapter.Capabilities(ctx, cwd)
}

func TestPeerModelsPreservesNativeMetadataAndProject(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &catalogAdapter{Adapter: fake.New(), cwds: make(chan string, 1)}
	a.Capability.Models = []adapter.Model{{ID: "opus", ResolvedModel: "claude-opus-5-5", Efforts: []string{"high", "max"}}}
	h := hub.New(st, map[string]adapter.Adapter{"claude": a}, hub.Config{MaxParallelPeerTurns: 3})
	defer h.Close()
	closeSession, sess := newTestSession(t, New(h, []string{"claude"}))
	defer closeSession()
	dir := t.TempDir()
	out := callTool(t, sess, "peer_models", map[string]any{"agent": "claude", "cwd": dir})
	if got := <-a.cwds; got != dir {
		t.Fatalf("cwd = %q, want %q", got, dir)
	}
	models := out["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %v", models)
	}
	m := models[0].(map[string]any)
	if m["id"] != "opus" || m["resolved_model"] != "claude-opus-5-5" || m["default_effort"] != "" {
		t.Fatalf("model metadata = %v", m)
	}
	efforts := m["efforts"].([]any)
	if len(efforts) != 2 || efforts[1] != "max" {
		t.Fatalf("efforts = %v", efforts)
	}
}

// The profiles are deliberately the reverse of the real providers'. An
// implementation that guesses from the agent name — which is what this
// replaced — passes only if the guess happens to match, so inverting them
// makes the regression detectable.
func TestPeerListReportsEachAgentsOwnProfile(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	codexLike := fake.New()
	codexLike.Prof = adapter.Profile{Steer: false, SandboxedReadOnly: false, Approvals: false}
	claudeLike := fake.New()
	claudeLike.Prof = adapter.Profile{Steer: true, SandboxedReadOnly: true, Approvals: true}

	h := hub.New(st, map[string]adapter.Adapter{
		"codex": codexLike, "claude": claudeLike,
	}, hub.Config{MaxParallelPeerTurns: 3})
	defer h.Close()
	f := New(h, []string{"codex", "claude"})

	closeSess, sess := newTestSession(t, f)
	defer closeSess()

	out := callTool(t, sess, "peer_list", map[string]any{})
	agents, ok := out["agents"].([]any)
	if !ok || len(agents) != 2 {
		t.Fatalf("agents = %v", out["agents"])
	}
	got := map[string]map[string]any{}
	for _, raw := range agents {
		a := raw.(map[string]any)
		got[a["name"].(string)] = a
	}
	if got["codex"]["steer"].(bool) {
		t.Error("peer_list must report the adapter's profile, not guess from the name")
	}
	if !got["claude"]["steer"].(bool) {
		t.Error("peer_list must report the adapter's profile, not guess from the name")
	}
	if !got["claude"]["approvals"].(bool) {
		t.Error("approvals must come from the profile too")
	}
}
