package doctor

import (
	"strings"
	"testing"
)

func TestDriftIsSilentOnTheVerifiedVersion(t *testing.T) {
	if n := DriftNote("codex", "0.154.0"); n != "" {
		t.Errorf("DriftNote on the verified version = %q, want none", n)
	}
}

func TestDriftNamesBothVersionsAndTheCheck(t *testing.T) {
	n := DriftNote("codex", "0.155.1")
	if n == "" {
		t.Fatal("a newer provider must produce a note")
	}
	for _, want := range []string{"0.155.1", "0.154.0", "generate-json-schema"} {
		if !strings.Contains(n, want) {
			t.Errorf("note %q is missing %q", n, want)
		}
	}
}

func TestDriftIgnoresAgentsWithNoRecordedVersion(t *testing.T) {
	if n := DriftNote("unknown-provider", "9.9.9"); n != "" {
		t.Errorf("DriftNote for an unrecorded agent = %q, want none", n)
	}
}

func TestDriftIgnoresAnUnknownVersion(t *testing.T) {
	// doctor reports an empty version when the probe failed; that is
	// already its own finding and must not become a second one.
	if n := DriftNote("codex", ""); n != "" {
		t.Errorf("DriftNote with no version = %q, want none", n)
	}
}
