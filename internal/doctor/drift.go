package doctor

import "fmt"

// verifiedAgainst records the provider version whose wire protocol the
// adapters were last checked against, field by field, using the schema
// the CLI generates for itself.
//
// The Codex app-server protocol is marked experimental and does change.
// Nothing else notices when it does: a renamed field decodes as a zero
// value and the adapter simply stops seeing something, with no error. So
// when the installed version moves past the recorded one, say so and name
// the check to re-run.
var verifiedAgainst = map[string]string{
	"codex":  "0.154.0",
	"claude": "2.1.267",
	"grok":   "1.0.25",
	"kimi":   "2.1.1",
}

// DriftNote returns a line asking for a re-check when the installed
// version differs from the one the adapters were verified against, or ""
// when there is nothing to say.
func DriftNote(agent, installed string) string {
	want, ok := verifiedAgainst[agent]
	if !ok || installed == "" || installed == want {
		return ""
	}
	if agent == "grok" {
		return fmt.Sprintf("grok %s differs from verified %s; re-run GROK_HUB_INTEGRATION=1 go test -tags integration ./internal/grok -run Real", installed, want)
	}
	if agent == "codex" {
		return fmt.Sprintf(
			"%s %s is newer than the verified %s. The app-server protocol is "+
				"experimental; re-check it with `codex app-server generate-json-schema "+
				"--out <dir>` and compare against internal/codex/protocol.go",
			agent, installed, want)
	}
	return fmt.Sprintf(
		"%s %s differs from the verified %s; re-run "+
			"`go test -tags integration ./internal/%s/ -run Real`",
		agent, installed, want, agent)
}
