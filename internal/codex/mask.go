package codex

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// MaskArgs returns the app-server flags that disable one MCP server for
// every thread in the process.
//
// A `-c` override merges into the configured table rather than replacing
// it, which is why this form sets only `enabled`. Supplying a transport
// here would collide with whatever the operator already configured:
//
//	failed to reload config: url is not supported for stdio
//
// The cost of the merge is that this form alone is invalid when the
// server is not configured at all, because the entry then has no
// transport and Codex refuses to start:
//
//	invalid transport in `mcp_servers.agent-hub`
//
// MaskArgsStandalone covers that case, and Open picks between them.
//
// Verified against codex-cli 0.153.4: with agent-hub registered over
// streamable HTTP, an unmasked process lists its twelve peer_* tools and a
// masked one lists none. Note that the server *name* stays in
// mcpServerStatus/list either way; the tool count is the real signal.
func MaskArgs(serverName string) []string {
	if serverName == "" {
		return nil
	}
	return []string{"-c", fmt.Sprintf("mcp_servers.%s.enabled=false", serverName)}
}

// MaskArgsStandalone disables a server that is not configured, by giving
// it a complete but disabled stdio entry.
func MaskArgsStandalone(serverName string) []string {
	if serverName == "" {
		return nil
	}
	return []string{"-c", fmt.Sprintf(
		`mcp_servers.%s={command="/bin/false", enabled=false}`, serverName)}
}

// Open starts an app-server child with maskServer hidden from it and
// completes the handshake.
//
// It tries the merge-safe mask first and falls back to the standalone form
// when Codex rejects the configuration. Deciding from the error rather
// than by inspecting config.toml means this keeps working wherever the
// operator's Codex configuration actually lives.
func Open(ctx context.Context, bin, dir, maskServer string, opts AdapterOptions) (*Adapter, error) {
	attempts := [][]string{MaskArgs(maskServer), MaskArgsStandalone(maskServer)}
	var lastErr error
	for i, mask := range attempts {
		args := append([]string{"app-server"}, mask...)
		conn, err := Dial(ctx, bin, args, dir)
		if err != nil {
			lastErr = err
			continue
		}
		// The same argv is remembered so a child that dies can be
		// replaced without rediscovering which mask form works.
		redial := func(c context.Context) (*Conn, error) { return Dial(c, bin, args, dir) }
		a, err := newAdapter(ctx, conn, opts, redial)
		if err == nil {
			return a, nil
		}
		_ = conn.Close()
		lastErr = err
		if i == 0 && looksLikeConfigRejection(err) {
			slog.Info("mask form rejected, retrying with a standalone entry",
				"server", maskServer, "err", err)
			continue
		}
		break
	}
	return nil, fmt.Errorf("open codex app-server: %w", lastErr)
}

func looksLikeConfigRejection(err error) bool {
	// The child dies during startup, so the handshake fails as a closed
	// connection rather than as a readable config error.
	msg := strings.ToLower(err.Error())
	for _, sign := range []string{
		"connection closed", // the child died during startup
		"invalid transport", // an entry with no command and no url
		"config error",      // Codex's own wording
		"reload config",     // "failed to reload config: ..."
		"not supported for", // "url is not supported for stdio"
	} {
		if strings.Contains(msg, sign) {
			return true
		}
	}
	return false
}
