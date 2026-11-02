package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func extensionResult(raw json.RawMessage) json.RawMessage {
	var env map[string]json.RawMessage
	if json.Unmarshal(raw, &env) == nil {
		if r, ok := env["result"]; ok {
			return r
		}
	}
	return raw
}

// Verify the live registry, not inspect's discovery-only report. Fail closed
// before any model turn if a forbidden plugin or recursive MCP route survives.
func verifyIsolation(ctx context.Context, r *rpc, id string) error {
	params := map[string]string{"sessionId": id}
	var raw json.RawMessage
	if err := r.call(ctx, "_x.ai/plugins/list", params, &raw); err != nil {
		return fmt.Errorf("grok plugin isolation check: %w", err)
	}
	var plugins struct {
		Plugins *[]struct {
			Name    string `json:"name"`
			Enabled *bool  `json:"enabled"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(extensionResult(raw), &plugins); err != nil || plugins.Plugins == nil {
		return fmt.Errorf("grok: unsupported plugin inventory response")
	}
	for _, p := range *plugins.Plugins {
		if p.Enabled == nil {
			return fmt.Errorf("grok: plugin %q has unknown activation state", p.Name)
		}
		if *p.Enabled && (p.Name == "superpowers" || p.Name == "codex") {
			return fmt.Errorf("grok: forbidden inherited plugin %q is active", p.Name)
		}
	}
	if err := r.call(ctx, "_x.ai/mcp/list", params, &raw); err != nil {
		return fmt.Errorf("grok MCP isolation check: %w", err)
	}
	var mcps struct {
		Servers *[]struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(extensionResult(raw), &mcps); err != nil || mcps.Servers == nil {
		return fmt.Errorf("grok: unsupported MCP inventory response")
	}
	for _, m := range *mcps.Servers {
		if strings.Contains(strings.ReplaceAll(strings.ToLower(m.Name), "_", "-"), "agent-hub") {
			return fmt.Errorf("grok: recursive MCP server %q is still present", m.Name)
		}
	}
	return nil
}
