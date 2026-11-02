package claude

import "testing"

// A representative system/init fixture with synthetic identity and paths.
// memory_paths is an object; when it was typed as a list the whole line
// failed to decode and the adapter reported no capabilities at all,
// without any error surfacing.
const realInitLine = `{"type":"system","subtype":"init","session_id":"11111111-2222-4333-8444-555555555555",` +
	`"model":"claude-haiku-4-5-20251001","cwd":"/tmp/p","permissionMode":"plan",` +
	`"claude_code_version":"2.1.267","tools":["Task","Bash","Edit","mcp__agent-hub__peer_start"],` +
	`"skills":["deep-research"],"agents":["Explore","Plan"],"slash_commands":["init"],` +
	`"memory_paths":{"auto":"/home/example/.claude/projects/-tmp/memory/"},` +
	`"mcp_servers":[{"name":"agent-hub","status":"connected"},{"name":"drive","status":"needs-auth"}],` +
	`"plugins":[{"name":"superpowers","path":"/x","source":"y","version":"6.3.0"}],` +
	`"capabilities":["interrupt_receipt_v1"],"output_style":"default","analytics_disabled":false}`

func TestDecodeRealInitLine(t *testing.T) {
	m := decode([]byte(realInitLine))
	if m.Type != MsgSystem || m.Subtype != "init" {
		t.Fatalf("type/subtype = %q/%q", m.Type, m.Subtype)
	}
	if m.Init == nil {
		t.Fatal("Init must be populated for a real init line")
	}
	i := m.Init
	if i.SessionID != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("SessionID = %q", i.SessionID)
	}
	if i.MemoryPaths["auto"] == "" {
		t.Errorf("MemoryPaths = %v, want the object decoded", i.MemoryPaths)
	}
	if len(i.Tools) != 4 {
		t.Errorf("Tools = %v", i.Tools)
	}
	if len(i.MCPServers) != 2 || i.MCPServers[0].Status != "connected" {
		t.Errorf("MCPServers = %+v", i.MCPServers)
	}
	if i.Version != "2.1.267" {
		t.Errorf("Version = %q", i.Version)
	}
}

func TestDecodeKeepsWhatItCanWhenAFieldChangesShape(t *testing.T) {
	// tools became a list of objects in this hypothetical future.
	line := `{"type":"system","subtype":"init","session_id":"abc","model":"m",` +
		`"tools":[{"name":"Bash"}],"mcp_servers":[{"name":"agent-hub","status":"connected"}]}`
	m := decode([]byte(line))
	if m.Init == nil {
		t.Fatal("a changed field must degrade the report, not discard it")
	}
	if m.Init.SessionID != "abc" {
		t.Errorf("SessionID = %q", m.Init.SessionID)
	}
}

func TestDominantModelPicksTheBiggestSpender(t *testing.T) {
	line := []byte(`{"type":"result","modelUsage":{
	  "claude-haiku-4-5":{"costUSD":0.001,"inputTokens":10,"outputTokens":5},
	  "claude-opus-5":{"costUSD":0.42,"inputTokens":900,"outputTokens":300},
	  "claude-sonnet-5":{"costUSD":0.05,"inputTokens":100,"outputTokens":50}}}`)
	if got := dominantModel(line); got != "claude-opus-5" {
		t.Errorf("dominantModel = %q, want claude-opus-5", got)
	}
}

func TestDominantModelIsStableWithoutCost(t *testing.T) {
	// No cost reported anywhere: the token count decides, and a tie is
	// broken by name so the answer never depends on map order.
	line := []byte(`{"type":"result","modelUsage":{
	  "b-model":{"inputTokens":5,"outputTokens":5},
	  "a-model":{"inputTokens":5,"outputTokens":5}}}`)
	first := dominantModel(line)
	for i := 0; i < 50; i++ {
		if got := dominantModel(line); got != first {
			t.Fatalf("dominantModel is unstable: %q then %q", first, got)
		}
	}
	if first != "a-model" {
		t.Errorf("tie broken to %q, want a-model", first)
	}
}

func TestDominantModelOnAnEmptyLine(t *testing.T) {
	if got := dominantModel([]byte(`{"type":"result"}`)); got != "" {
		t.Errorf("dominantModel = %q, want empty", got)
	}
}
