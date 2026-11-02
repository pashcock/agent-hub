package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapabilitiesUsesFreshCatalogWithoutUserTurn(t *testing.T) {
	dir := t.TempDir()
	memo := filepath.Join(dir, "user-message")
	catalog := filepath.Join(dir, "catalog.json")
	t.Setenv("FAKECLAUDE_MEMO", memo)
	t.Setenv("FAKECLAUDE_CATALOG", catalog)
	a := newAdapter(t, Options{})
	for _, resolved := range []string{"claude-opus-5-5", "future-provider-model"} {
		data := `{"models":[{"value":"opus","resolvedModel":"` + resolved + `","supportedEffortLevels":["low","high"]},{"value":"unavailable","disabled":true},{"value":"custom-model"}]}`
		if err := os.WriteFile(catalog, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		caps, err := a.Capabilities(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(caps.Models) != 2 || caps.Models[0].ID != "opus" || caps.Models[0].ResolvedModel != resolved {
			t.Fatalf("models = %+v", caps.Models)
		}
		if len(caps.Models[0].Efforts) != 2 || caps.Models[0].DefaultEffort != "" || len(caps.Models[1].Efforts) != 0 {
			t.Fatalf("invented or missing effort metadata: %+v", caps.Models)
		}
		if !caps.MCPConnected["agent-hub"] || caps.MCPConnected["offline"] || len(caps.MCPServers) != 2 {
			t.Fatalf("MCP status = %+v", caps)
		}
		if _, err := os.Stat(memo); !os.IsNotExist(err) {
			t.Fatalf("probe sent a user turn: stat = %v", err)
		}
		if a.LiveProcesses() != 0 {
			t.Fatal("probe leaked a process slot")
		}
	}
}

func TestParseModelsRejectsMissingCatalog(t *testing.T) {
	for _, raw := range []string{`{`, `{}`, `{"models":null}`, `{"models":"invalid"}`} {
		if _, err := parseModels([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	models, err := parseModels([]byte(`{"models":[]}`))
	if err != nil || len(models) != 0 {
		t.Fatalf("empty catalog: %v, %v", models, err)
	}
}

func TestCatalogControlFailures(t *testing.T) {
	bin := buildFakeClaude(t)
	for _, mode := range []string{"error", "hang", "exit"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("FAKECLAUDE_CONTROL_MODE", mode)
			p := spawnFake(t, SpawnOptions{Bin: bin})
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := p.control(ctx, "initialize")
			if err == nil {
				t.Fatal("control failure was ignored")
			}
			if mode == "error" && !strings.Contains(err.Error(), "unsupported control request") {
				t.Fatalf("lost provider error: %v", err)
			}
			p.ctrl.Lock()
			pending := len(p.awaits)
			p.ctrl.Unlock()
			if pending != 0 {
				t.Fatalf("leaked %d pending requests", pending)
			}
		})
	}
}
