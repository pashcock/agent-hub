package webui

import (
	"encoding/json"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
	"testing"
)

func TestStatusAccountingWire(t *testing.T) {
	u := store.Usage{InputTokens: 100, CachedInput: 80, TotalTokens: 110}
	out := statusFrom(store.Conversation{PeerAgent: "grok"}, hub.PollResult{Usage: &u,
		Conversation: &hub.ConversationAccounting{Usage: hub.ConsumptionOf("grok", u), AccountingCoverage: store.AccountingCoverage{Turns: 2}}})
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	usage := v["usage"].(map[string]any)
	conv := v["conversation"].(map[string]any)
	if usage["cached_input_tokens"] != float64(80) || usage["cache_hit_percent"] != float64(80) || conv["turns"] != float64(2) || conv["execution_ms"] != nil {
		t.Fatalf("wire: %s", raw)
	}
}
