package doctor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestRunReportsFoundAndMissing(t *testing.T) {
	look := func(bin string) (string, error) {
		if bin == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", exec.ErrNotFound
	}
	run := func(_ context.Context, bin string, _ ...string) (string, error) {
		if bin == "/usr/bin/codex" {
			return "codex-cli 0.153.4\n", nil
		}
		return "", errors.New("not executed")
	}

	rep := Run(context.Background(), look, run)

	codex := rep.Find("codex")
	if codex == nil {
		t.Fatal("report must contain a codex finding")
	}
	if !codex.Installed {
		t.Error("codex must be reported installed")
	}
	if codex.Version != "0.153.4" {
		t.Errorf("codex version = %q, want 0.153.4", codex.Version)
	}
	if codex.Protocol != "app-server" {
		t.Errorf("codex protocol = %q, want app-server", codex.Protocol)
	}

	kimi := rep.Find("kimi")
	if kimi == nil {
		t.Fatal("report must contain a kimi finding")
	}
	if kimi.Installed {
		t.Error("kimi must be reported missing")
	}
}

func TestStringIsAligned(t *testing.T) {
	look := func(string) (string, error) { return "", exec.ErrNotFound }
	run := func(context.Context, string, ...string) (string, error) { return "", nil }
	out := Run(context.Background(), look, run).String()
	for _, name := range []string{"codex", "claude", "kimi", "grok"} {
		if !strings.Contains(out, name) {
			t.Errorf("report output missing %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "not installed") {
		t.Errorf("report output must mark missing agents:\n%s", out)
	}
}

func TestParseVersionExtractsSemver(t *testing.T) {
	cases := map[string]string{
		"codex-cli 0.153.4\n":   "0.153.4",
		"2.1.265 (Claude Code)": "2.1.265",
		"no digits here":        "",
	}
	for in, want := range cases {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKimiFallbackPath(t *testing.T) {
	look := func(bin string) (string, error) {
		if strings.HasSuffix(bin, "/.kimi-code/bin/kimi") {
			return bin, nil
		}
		return "", exec.ErrNotFound
	}
	run := func(context.Context, string, ...string) (string, error) { return "2.1.1", nil }
	f := Run(context.Background(), look, run).Find("kimi")
	if !f.Installed || f.Version != "2.1.1" || f.Note != "" {
		t.Fatal(f)
	}
}
