package grok

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

func TestControlRenewsAuthWithoutStartingAnUnsandboxedSession(t *testing.T) {
	a := fakeAdapter(t)
	root := t.TempDir()
	trace := filepath.Join(root, "trace")
	state := filepath.Join(root, "auth-state")
	t.Setenv("AGENT_HUB_GROK_TRACE", trace)
	t.Setenv("AGENT_HUB_GROK_AUTH_STATE", state)
	os.WriteFile(state, []byte("expired"), 0600)
	project := t.TempDir()
	caps, err := a.Capabilities(context.Background(), project)
	if err != nil || len(caps.Models) != 2 {
		t.Fatalf("discovery: %+v, %v", caps, err)
	}
	if b, _ := os.ReadFile(state); string(b) != "renewed" {
		t.Fatal("expired credential was not renewed")
	}
	id, err := a.StartSession(context.Background(), adapter.StartOptions{Cwd: project, Mode: adapter.ModeReview, Model: "test-grok"})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := a.Events(id)
	sendFake(t, a, id, "hello")
	waitEvent(t, ch, adapter.EventTurnCompleted)
	b, _ := os.ReadFile(trace)
	var controlHome string
	var sawPrompt bool
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev struct {
			Method, Cwd, Home, Sandbox string
			Control                    bool
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Control {
			if ev.Cwd == project || ev.Cwd != ev.Home || ev.Home == a.opts.Home {
				t.Fatalf("unsafe service cwd: %+v", ev)
			}
			switch ev.Method {
			case "initialize", "authenticate", "_x.ai/models/list":
			default:
				t.Fatalf("service called %s", ev.Method)
			}
			controlHome = ev.Home
		} else {
			if ev.Sandbox != "read-only" || ev.Cwd != project {
				t.Fatalf("lost sandbox: %+v", ev)
			}
			if ev.Method == "session/prompt" {
				sawPrompt = true
			}
		}
	}
	if controlHome == "" || !sawPrompt {
		t.Fatal("missing service or model traffic")
	}
	if _, err := os.Stat(controlHome); !os.IsNotExist(err) {
		t.Fatalf("service home retained: %v", err)
	}
}

func TestControlRejectsFallbackAndBlocksTurnsOnRefreshFailure(t *testing.T) {
	a := fakeAdapter(t)
	id, _ := startFake(t, a)
	before, _ := os.ReadFile(filepath.Join(a.opts.Home, "models_cache.json"))
	t.Setenv("AGENT_HUB_GROK_NO_CATALOG", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := a.Capabilities(ctx, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "refusing an unverified fallback") {
		t.Fatalf("fallback accepted: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(a.opts.Home, "models_cache.json"))
	if string(after) != string(before) {
		t.Fatal("failed discovery replaced the verified runtime cache")
	}
	// Failed discovery must invalidate the recent auth shortcut.
	t.Setenv("AGENT_HUB_GROK_AUTH_FAIL", "1")
	if _, err := a.SendTurn(context.Background(), id, "must not run", adapter.TurnOptions{}); err == nil {
		t.Fatal("sent turn without auth recovery")
	}
	if a.hasActiveTurn() {
		t.Fatal("failed preflight created a turn")
	}
	if _, err := a.StartSession(context.Background(), adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeCode}); err == nil {
		t.Fatal("session started without auth recovery")
	}
}

func TestControlCancelledProbeAndShutdownReleaseProcess(t *testing.T) {
	a := fakeAdapter(t)
	t.Setenv("AGENT_HUB_GROK_AUTH_HANG", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.Capabilities(ctx, t.TempDir()); err == nil {
		t.Fatal("hung probe succeeded")
	}
	if used, _ := a.Capacity(); used != 0 {
		t.Fatalf("leaked capacity: %d", used)
	}
	result := make(chan error, 1)
	go func() { _, err := a.Capabilities(context.Background(), ""); result <- err }()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed adapter accepted probe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel service probe")
	}
}

func TestCatalogEvidenceRejectsStalePartialAndForeignAuth(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		age          time.Duration
		ids          []string
	}{
		{"stale", "session", -time.Hour, []string{"test-grok"}},
		{"wrong-auth", "api-key", 0, []string{"test-grok"}},
		{"fallback", "session", 0, []string{"test-grok", "future-model"}},
		{"unknown-model", "session", 0, []string{"future-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			now := time.Now()
			entries := map[string]any{}
			for _, id := range tc.ids {
				entries[id] = map[string]any{"info": map[string]any{"hidden": false}}
			}
			b, _ := json.Marshal(map[string]any{"fetched_at": now.Add(tc.age), "auth_method": tc.method, "models": entries})
			os.WriteFile(filepath.Join(home, "models_cache.json"), b, 0600)
			if err := verifyCatalogCache(home, now, []adapter.Model{{ID: "test-grok"}}); err == nil {
				t.Fatal("bad catalog evidence accepted")
			}
		})
	}
}

func TestActiveTurnCanReadCredentialsRenewedByService(t *testing.T) {
	a := fakeAdapter(t)
	state := filepath.Join(t.TempDir(), "auth-state")
	t.Setenv("AGENT_HUB_GROK_AUTH_STATE", state)
	id, ch := startFake(t, a)
	os.WriteFile(state, []byte("expired"), 0600)
	sendFake(t, a, id, "wait-for-renewed-auth")
	waitEvent(t, ch, adapter.EventTurnStarted)
	if !a.hasActiveTurn() {
		t.Fatal("running turn not detected")
	}
	a.controlMu.Lock()
	a.lastAuth = time.Now().Add(-2 * authRefreshInterval)
	a.controlMu.Unlock()
	if _, err := a.controlCatalog(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(state); string(b) != "renewed" {
		t.Fatal("active turn auth not renewed")
	}
	waitEvent(t, ch, adapter.EventTurnCompleted)
	s, _ := a.get(id)
	s.mu.Lock()
	sandbox := s.proc.cmd.Env
	s.mu.Unlock()
	if !strings.Contains(strings.Join(sandbox, "\n"), "GROK_SANDBOX=workspace") {
		t.Fatal("model sandbox disabled")
	}
}

func TestControlEnvironmentExcludesExecutionAndBillingOverrides(t *testing.T) {
	a := fakeAdapter(t)
	for _, key := range []string{"GROK_AUTH_PROVIDER_COMMAND", "GROK_AGENT_PROFILE", "GROK_MODELS_BASE_URL", "GROK_CONFIG", "XAI_API_KEY", "GROK_API_KEY", "AGENT_HUB_TOKEN"} {
		t.Setenv(key, "must-not-inherit")
	}
	env := strings.Join(a.controlEnvironment(), "\n")
	if strings.Contains(env, "must-not-inherit") {
		t.Fatal("service inherited unsafe overrides")
	}
	for _, expected := range []string{"GROK_SANDBOX=off", "GROK_AUTH_PATH=" + a.opts.AuthPath, "GROK_DISABLE_API_KEY_AUTH=true", "GROK_CLAUDE_HOOKS_ENABLED=false"} {
		if !strings.Contains(env, expected) {
			t.Fatalf("missing %s", expected)
		}
	}
}

func TestControlWaitsForCatalogAndPublishesVerifiedCache(t *testing.T) {
	a := fakeAdapter(t)
	t.Setenv("AGENT_HUB_GROK_CONTROL_DELAY", "1")
	started := time.Now()
	caps, err := a.Capabilities(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCatalogCache(a.opts.Home, started, caps.Models); err != nil {
		t.Fatalf("runtime did not receive verified cache: %v", err)
	}
}
