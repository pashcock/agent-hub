package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
)

const authRefreshInterval = time.Minute

// controlCatalog is the only unsandboxed Grok path. It runs in an empty private
// directory, never creates/loads a session or sends a prompt, and rejects every
// server request. Only Grok itself handles subscription credentials and refresh.
// The model processes retain their original sandbox and shared auth path.
func (a *Adapter) controlCatalog(ctx context.Context, fresh bool) ([]adapter.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stop := context.AfterFunc(a.controlCtx, cancel)
	defer stop()
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	if err := a.controlCtx.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !fresh && !a.lastAuth.IsZero() && time.Since(a.lastAuth) < authRefreshInterval {
		return nil, nil
	}
	// A failed discovery invalidates a previous success, so a subsequent turn
	// cannot skip recovery just because that success was recent.
	a.lastAuth = time.Time{}
	home, err := os.MkdirTemp("", "agent-hub-grok-control-")
	if err != nil {
		return nil, fmt.Errorf("grok: service directory: %w", err)
	}
	defer os.RemoveAll(home)
	profile := &Adapter{opts: Options{Home: home, AuthPath: a.opts.AuthPath}}
	if err := profile.prepare(); err != nil {
		return nil, err
	}
	env := profile.controlEnvironment()
	started := time.Now()
	r, err := dial(a.opts.Bin, home, []string{"agent", "--no-leader", "stdio"}, env,
		func(*rpc, string, json.RawMessage) {},
		func(r *rpc, id json.RawMessage, _ string, _ json.RawMessage) {
			_ = r.reject(id, "service probe cannot execute tools or approve permissions")
		})
	if err != nil {
		return nil, fmt.Errorf("grok: service probe: %w", err)
	}
	defer r.close()
	if err := r.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientInfo":         map[string]string{"name": "agent-hub-control", "version": "0.1.0"},
		"clientCapabilities": map[string]any{},
	}, nil); err != nil {
		return nil, fmt.Errorf("grok: service initialize: %w", err)
	}
	if err := r.call(ctx, "authenticate", map[string]any{
		"methodId": "cached_token", "_meta": map[string]any{"headless": true},
	}, nil); err != nil {
		return nil, fmt.Errorf("grok: subscription authentication failed: %w", err)
	}
	for {
		var raw json.RawMessage
		if err := r.call(ctx, "_x.ai/models/list", map[string]any{}, &raw); err != nil {
			return nil, fmt.Errorf("grok: model discovery failed: %w", err)
		}
		models, err := parseModels(raw)
		if err != nil {
			return nil, err
		}
		// Startup can answer with the built-in catalog before the asynchronous
		// fetch finishes. Wait for fresh evidence, never advertise that fallback.
		data, evidenceErr := os.ReadFile(filepath.Join(home, "models_cache.json"))
		if evidenceErr == nil {
			evidenceErr = verifyCatalogData(data, started, models)
		}
		if evidenceErr == nil {
			if err := a.publishCatalog(data); err != nil {
				return nil, err
			}
			a.lastAuth = time.Now()
			return models, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("grok: live model catalog unavailable; refusing an unverified fallback: %v: %w", evidenceErr, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (a *Adapter) controlEnvironment() []string {
	env := []string{}
	for _, entry := range a.environment(adapter.StartOptions{Mode: adapter.ModeAsk}) {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GROK_") {
			switch key {
			case "GROK_HOME", "GROK_AUTH_PATH":
			default:
				// Do not inherit auth commands, plugin/profile overrides, custom
				// model endpoints or sandbox overrides into this service process.
				if !strings.HasSuffix(key, "_ENABLED") ||
					!(strings.HasPrefix(key, "GROK_CLAUDE_") || strings.HasPrefix(key, "GROK_CURSOR_") || strings.HasPrefix(key, "GROK_CODEX_")) {
					continue
				}
			}
		}
		env = append(env, entry)
	}
	return append(env, "GROK_SANDBOX=off", "GROK_DISABLE_API_KEY_AUTH=true")
}

func verifyCatalogCache(home string, started time.Time, models []adapter.Model) error {
	data, err := os.ReadFile(filepath.Join(home, "models_cache.json"))
	if err != nil {
		return fmt.Errorf("native catalog cache was not written: %w", err)
	}
	return verifyCatalogData(data, started, models)
}

func verifyCatalogData(data []byte, started time.Time, models []adapter.Model) error {
	var cache struct {
		FetchedAt  time.Time `json:"fetched_at"`
		AuthMethod string    `json:"auth_method"`
		Models     map[string]struct {
			Info struct {
				Hidden bool `json:"hidden"`
			} `json:"info"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &cache) != nil || cache.AuthMethod != "session" ||
		cache.FetchedAt.Before(started.Add(-time.Second)) || cache.FetchedAt.After(time.Now().Add(time.Second)) || len(cache.Models) == 0 {
		return fmt.Errorf("native catalog cache is missing fresh subscription metadata (unsupported or failed discovery)")
	}
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		entry, ok := cache.Models[m.ID]
		if !ok || entry.Info.Hidden || seen[m.ID] {
			return fmt.Errorf("ACP catalog does not match the fetched catalog")
		}
		seen[m.ID] = true
	}
	for id, entry := range cache.Models {
		if !entry.Info.Hidden && !seen[id] {
			return fmt.Errorf("ACP catalog omits an available model from the fetched catalog")
		}
	}
	return nil
}

// Refresh outside the model sandbox while a turn is running. Grok's native auth
// manager can read the renewed shared credentials without granting model tools
// write access to the user's credential directory. Never replay a failed turn.
func (a *Adapter) maintainAuth() {
	ticker := time.NewTicker(authRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			if a.hasActiveTurn() {
				if _, err := a.controlCatalog(a.controlCtx, false); err != nil && a.controlCtx.Err() == nil {
					slog.Warn("grok service authentication refresh failed", "err", err)
				}
			}
		}
	}
}

func (a *Adapter) hasActiveTurn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		s.mu.Lock()
		active := !s.closed && s.turn != ""
		s.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

// Publish only the bytes verified above. Native task processes can bootstrap
// from the same account catalog instead of independently racing a network fetch.
func (a *Adapter) publishCatalog(data []byte) error {
	if err := a.prepare(); err != nil {
		return err
	}
	f, err := os.CreateTemp(a.opts.Home, ".models-cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(a.opts.Home, "models_cache.json")); err != nil {
		return fmt.Errorf("grok: publish verified catalog: %w", err)
	}
	return nil
}
