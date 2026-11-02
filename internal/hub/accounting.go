package hub

import "github.com/pashcock/agent-hub/internal/store"

// Consumption preserves raw provider input semantics and exposes a comparable
// input total. Cache reads are a subset, never extra tokens to add to total.
type Consumption struct {
	Model               string   `json:"model,omitempty"`
	InputTokens         int      `json:"input_tokens"`
	InputIncludingCache int      `json:"input_including_cache"`
	OutputTokens        int      `json:"output_tokens"`
	TotalTokens         int      `json:"total_tokens"`
	CachedInputTokens   int      `json:"cached_input_tokens"`
	CacheWriteTokens    int      `json:"cache_write_tokens"`
	ReasoningTokens     int      `json:"reasoning_tokens"`
	CacheHitPercent     *float64 `json:"cache_hit_percent"`
	CostUSD             float64  `json:"cost_usd"`
}

func ConsumptionOf(agent string, u store.Usage) Consumption {
	input := u.InputTokens
	if agent == "claude" {
		input += u.CachedInput + u.CacheWrite
	}
	c := Consumption{Model: u.Model, InputTokens: u.InputTokens, InputIncludingCache: input,
		OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens, CachedInputTokens: u.CachedInput,
		CacheWriteTokens: u.CacheWrite, ReasoningTokens: u.ReasoningTokens, CostUSD: u.CostUSD}
	if input > 0 && u.CachedInput <= input {
		p := 100 * float64(u.CachedInput) / float64(input)
		c.CacheHitPercent = &p
	}
	return c
}

type ConversationAccounting struct {
	Usage Consumption `json:"usage"`
	store.AccountingCoverage
	// Reported snapshots may be incomplete while a turn runs or on provider error.
	Scope              string `json:"scope"`
	SubagentAccounting string `json:"subagent_accounting"`
}
