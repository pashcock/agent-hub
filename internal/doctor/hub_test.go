package doctor

import (
	"errors"
	"strings"
	"testing"
)

func TestHubCheckReportsAMatchingToken(t *testing.T) {
	got := CheckHub(HubInputs{
		Addr:      "127.0.0.1:7777",
		TokenFile: "/state/token",
		ReadToken: func(string) (string, error) { return "abc123", nil },
		Getenv:    func(string) string { return "abc123" },
		Probe:     func(string, string) (int, error) { return 200, nil },
	})
	if !got.TokenReadable || !got.EnvSet || !got.EnvMatches || !got.Authorized {
		t.Fatalf("check = %+v, want everything true", got)
	}
	if got.Problem() != "" {
		t.Errorf("Problem = %q, want none", got.Problem())
	}
}

func TestHubCheckNamesTheMissingEnvVar(t *testing.T) {
	got := CheckHub(HubInputs{
		Addr:      "127.0.0.1:7777",
		TokenFile: "/state/token",
		ReadToken: func(string) (string, error) { return "abc123", nil },
		Getenv:    func(string) string { return "" },
		Probe:     func(string, string) (int, error) { return 401, nil },
	})
	if got.EnvSet {
		t.Error("EnvSet must be false")
	}
	// The failure this catches is silent: an agent without the variable
	// simply never connects, with no error anywhere.
	if p := got.Problem(); p == "" || !strings.Contains(p, "AGENT_HUB_TOKEN") {
		t.Errorf("Problem = %q, want it to name the variable", p)
	}
}

func TestHubCheckSpotsAStaleEnvVar(t *testing.T) {
	got := CheckHub(HubInputs{
		Addr:      "127.0.0.1:7777",
		TokenFile: "/state/token",
		ReadToken: func(string) (string, error) { return "new-token", nil },
		Getenv:    func(string) string { return "old-token" },
		Probe:     func(string, string) (int, error) { return 401, nil },
	})
	if got.EnvMatches {
		t.Error("EnvMatches must be false for a stale value")
	}
	if p := got.Problem(); !strings.Contains(p, "does not match") {
		t.Errorf("Problem = %q, want it to say the value is stale", p)
	}
}

func TestHubCheckReportsAnUnreachableDaemon(t *testing.T) {
	got := CheckHub(HubInputs{
		Addr:      "127.0.0.1:7777",
		TokenFile: "/state/token",
		ReadToken: func(string) (string, error) { return "abc", nil },
		Getenv:    func(string) string { return "abc" },
		Probe:     func(string, string) (int, error) { return 0, errors.New("connection refused") },
	})
	if got.Reachable {
		t.Error("Reachable must be false")
	}
	if p := got.Problem(); !strings.Contains(p, "not running") {
		t.Errorf("Problem = %q, want it to say the daemon is not running", p)
	}
}

func TestHubCheckReportsAMissingTokenFile(t *testing.T) {
	got := CheckHub(HubInputs{
		Addr:      "127.0.0.1:7777",
		TokenFile: "/state/token",
		ReadToken: func(string) (string, error) { return "", errors.New("no such file") },
		Getenv:    func(string) string { return "" },
		Probe:     func(string, string) (int, error) { return 0, errors.New("connection refused") },
	})
	if got.TokenReadable {
		t.Error("TokenReadable must be false")
	}
	if p := got.Problem(); !strings.Contains(p, "never started") {
		t.Errorf("Problem = %q, want it to explain the daemon has not run", p)
	}
}
