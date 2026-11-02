//go:build integration

package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRealKimiNativePolicyAndProjectMCP(t *testing.T) {
	r := realHub(t)
	// Copy only account config into a fixture source, avoiding unrelated live MCPs.
	source := t.TempDir()
	data, err := os.ReadFile(filepath.Join(r.a.opts.SourceHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "config.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(r.a.opts.SourceHome, "credentials"), filepath.Join(source, "credentials")); err != nil {
		t.Fatal(err)
	}
	r.a.opts.SourceHome = source
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture-search", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "lookup", Description: "Return the fixture code search marker."}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "MCP-SEARCH-7412"}}}, nil, nil
	})
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer func() { r.a.Close(); httpSrv.Close() }()
	dir := filepath.Join(r.work, ".kimi-code")
	if err = os.MkdirAll(filepath.Join(dir, "plugins"), 0700); err != nil {
		t.Fatal(err)
	}
	// Deliberately conflicting defaults: the adapter must select yolo and pin guard on.
	if err = os.WriteFile(filepath.Join(dir, "local.toml"), []byte("default_permission_mode = \"auto\"\n[permission]\ndangerous_command_guard = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture-search": map[string]string{"url": httpSrv.URL}}})
	if err = os.WriteFile(filepath.Join(dir, "mcp.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	p := r.call(t, "peer_start", map[string]any{"agent": "kimi", "cwd": r.work, "mode": "code", "effort": "low", "task": "Do NOT use the superpowers plugin or any of its skills. Use Write to create normal.txt containing NORMAL. Use Bash to run printf NATIVE-OK. Call fixture-search MCP lookup and report its marker. Use select_tools to discover the tool if needed. Do not use subagents."})
	id := p["conversation_id"].(string)
	out := r.wait(t, id)
	if out["turn_status"] != "completed" || !strings.Contains(out["result"].(string), "MCP-SEARCH-7412") {
		t.Fatalf("routine tools/MCP: %v", out)
	}
	data, err = os.ReadFile(filepath.Join(r.work, "normal.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "NORMAL" {
		t.Fatalf("write: %s %v", data, err)
	}
	if err = os.Mkdir(filepath.Join(r.work, "guard-target"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(r.work, "guard-target", "marker")
	if err = os.WriteFile(marker, []byte("PRESERVE"), 0600); err != nil {
		t.Fatal(err)
	}
	r.call(t, "peer_send", map[string]any{"conversation_id": id, "message": "Do NOT use the superpowers plugin or any of its skills. This is an approval test on disposable fixture files. Invoke Bash exactly once with command rm -rf guard-target. Let the native permission request happen. If denied, stop and report DENIED. Do not use any alternate tool or command."})
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		out = r.call(t, "peer_poll", map[string]any{"conversation_id": id})
		switch out["turn_status"] {
		case "needs_permission":
			req := out["pending_approval"].(map[string]any)
			if !strings.Contains(fmt.Sprint(req["command"]), "rm -rf guard-target") {
				t.Fatalf("wrong approval: %v", req)
			}
			// Native approvals use kp_<session>; client fallback approvals use kp_<uuid>.
			if !strings.HasPrefix(req["id"].(string), "kp_"+p["native_session_id"].(string)+"_") {
				t.Fatalf("not native approval: %v", req)
			}
			r.call(t, "peer_approve", map[string]any{"conversation_id": id, "request_id": req["id"], "decision": "decline"})
			out = r.wait(t, id)
			if out["turn_status"] != "completed" {
				t.Fatalf("decline completion: %v", out)
			}
			if _, err = os.Stat(marker); err != nil {
				t.Fatal("declined command executed", err)
			}
			r.call(t, "peer_close", map[string]any{"conversation_id": id})
			return
		case "completed", "failed", "cancelled":
			t.Fatalf("native approval missing: %v", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("native approval timeout")
}
