package codex

import (
	"strings"
	"testing"
)

func TestMaskArgsSetsOnlyEnabled(t *testing.T) {
	args := MaskArgs("agent-hub")
	if len(args) != 2 || args[0] != "-c" {
		t.Fatalf("MaskArgs must produce a -c pair, got %v", args)
	}
	if args[1] != "mcp_servers.agent-hub.enabled=false" {
		t.Errorf("override = %q", args[1])
	}
	// Supplying a transport here would collide with a configured one.
	if strings.Contains(args[1], "command=") || strings.Contains(args[1], "url=") {
		t.Errorf("the merge-safe form must not carry a transport, got %q", args[1])
	}
}

func TestMaskArgsStandaloneCarriesATransport(t *testing.T) {
	args := MaskArgsStandalone("agent-hub")
	if len(args) != 2 || args[0] != "-c" {
		t.Fatalf("got %v", args)
	}
	if !strings.Contains(args[1], "command=") {
		t.Errorf("an unconfigured server needs a transport, got %q", args[1])
	}
	if !strings.Contains(args[1], "enabled=false") {
		t.Errorf("the entry must still be disabled, got %q", args[1])
	}
}

func TestMaskArgsAreEmptyForAnEmptyName(t *testing.T) {
	if args := MaskArgs(""); len(args) != 0 {
		t.Errorf("MaskArgs(\"\") = %v", args)
	}
	if args := MaskArgsStandalone(""); len(args) != 0 {
		t.Errorf("MaskArgsStandalone(\"\") = %v", args)
	}
}

func TestLooksLikeConfigRejection(t *testing.T) {
	cases := map[string]bool{
		"codex initialize: call initialize: codex: connection closed": true,
		"invalid transport in `mcp_servers.agent-hub`":                true,
		"failed to reload config: url is not supported for stdio":     true,
		"model/list: some unrelated failure":                          false,
	}
	for msg, want := range cases {
		if got := looksLikeConfigRejection(errString(msg)); got != want {
			t.Errorf("looksLikeConfigRejection(%q) = %v, want %v", msg, got, want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
