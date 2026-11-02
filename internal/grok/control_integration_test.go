//go:build integration

package grok

import (
	"context"
	"github.com/pashcock/agent-hub/internal/adapter"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This test performs native auth/catalog requests only, never a model turn.
func TestRealControlCatalog(t *testing.T) {
	if os.Getenv("GROK_HUB_CONTROL_INTEGRATION") != "1" {
		t.Skip("set GROK_HUB_CONTROL_INTEGRATION=1 for native auth/catalog checks")
	}
	bin, err := exec.LookPath("grok")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	a := NewAdapter(Options{Bin: bin, Home: t.TempDir(), AuthPath: filepath.Join(home, ".grok", "auth.json")})
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	caps, err := a.Capabilities(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(caps.Models) == 0 {
		t.Fatal("empty live catalog")
	}
	for _, model := range caps.Models {
		t.Logf("verified native model: %s", model.ID)
	}
	if len(a.sessions) != 0 {
		t.Fatal("discovery created a model session")
	}
	if used, _ := a.Capacity(); used != 0 {
		t.Fatalf("probe retained capacity: %d", used)
	}
	t.Run("missing-login-is-not-a-catalog", func(t *testing.T) {
		missing := NewAdapter(Options{Bin: bin, Home: t.TempDir(), AuthPath: filepath.Join(t.TempDir(), "missing-auth.json")})
		defer missing.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		caps, err := missing.Capabilities(ctx, "")
		if err == nil || len(caps.Models) != 0 {
			t.Fatalf("unauthenticated fallback accepted: %+v, %v", caps, err)
		}
	})
}

// Session setup must honor the requested model too; discovery alone is not proof.
// No prompt or model turn is sent.
func TestRealGrok47SessionSelection(t *testing.T) {
	if os.Getenv("GROK_HUB_CONTROL_INTEGRATION") != "1" {
		t.Skip("native session setup opt-in")
	}
	bin, err := exec.LookPath("grok")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	a := NewAdapter(Options{Bin: bin, Home: t.TempDir(), AuthPath: filepath.Join(home, ".grok", "auth.json")})
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, model := range []string{"grok-4.7", ""} {
		name := model
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			id, err := a.StartSession(ctx, adapter.StartOptions{Cwd: t.TempDir(), Mode: adapter.ModeReview, Model: model})
			if err != nil {
				t.Fatal(err)
			}
			s, _ := a.get(id)
			if s.opts.Model != "grok-4.7" {
				t.Fatalf("model=%q", s.opts.Model)
			}
			if err := a.CloseSession(ctx, id); err != nil {
				t.Fatal(err)
			}
			if used, _ := a.Capacity(); used != 0 {
				t.Fatalf("closed session retained %d slots", used)
			}
		})
	}
}
