package webui

import (
	"github.com/pashcock/agent-hub/internal/store"
	"testing"
)

func TestCachedInputProviderSemantics(t *testing.T) {
	cached := funcs()["cachedInput"].(func(string, store.Usage) string)
	uncached := funcs()["uncachedInput"].(func(string, store.Usage) string)
	for _, agent := range []string{"grok", "codex", "claude"} {
		u := store.Usage{InputTokens: 1000, CachedInput: 800}
		if agent == "claude" {
			u.InputTokens = 150
			u.CacheWrite = 50
		}
		if got := cached(agent, u); got != "800 (80.0%)" {
			t.Fatalf("%s: %s", agent, got)
		}
		if got := uncached(agent, u); got != "200" {
			t.Fatalf("%s: %s", agent, got)
		}
	}
	if got := cached("grok", store.Usage{}); got != "0" {
		t.Fatal(got)
	}
	if got := uncached("grok", store.Usage{CachedInput: 100}); got != "—" {
		t.Fatal(got)
	}
}
