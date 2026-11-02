package claude

import (
	"encoding/json"
	"fmt"

	"github.com/pashcock/agent-hub/internal/adapter"
)

// parseModels reads the SDK initialize response, not system/init (which
// describes only the selected model and requires a user turn).
func parseModels(raw json.RawMessage) ([]adapter.Model, error) {
	var result struct {
		Models []struct {
			Value                 string   `json:"value"`
			ResolvedModel         string   `json:"resolvedModel"`
			SupportedEffortLevels []string `json:"supportedEffortLevels"`
			Disabled              bool     `json:"disabled"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("claude: initialize models: %w", err)
	}
	if result.Models == nil {
		return nil, fmt.Errorf("claude: initialize missing models (unsupported protocol)")
	}
	out := []adapter.Model{}
	for _, m := range result.Models {
		if m.Disabled || m.Value == "" {
			continue
		}
		// Preserve native aliases such as opusplan: replacing them with
		// a resolved id could change their behavior. The CLI does not
		// report per-model default effort, so leave it unknown.
		out = append(out, adapter.Model{
			ID: m.Value, ResolvedModel: m.ResolvedModel,
			Efforts: m.SupportedEffortLevels,
		})
	}
	return out, nil
}
