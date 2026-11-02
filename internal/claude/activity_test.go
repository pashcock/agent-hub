package claude

import (
	"encoding/json"
	"testing"
)

func TestToolActivityNamesTheToolAndItsArgument(t *testing.T) {
	cases := map[string]string{
		`{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/a/README.md"}}]}`:                  "Read: /tmp/a/README.md",
		`{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./...","description":"tests"}}]}`: "Bash: go test ./...",
		`{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"func main"}}]}`:                           "Grep: func main",
		`{"content":[{"type":"tool_use","name":"Task","input":{}}]}`:                                                "Task",
		// The most recent call in a message is the one being run.
		`{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/a"}},{"type":"tool_use","name":"Edit","input":{"file_path":"/b"}}]}`: "Edit: /b",
		// Nothing to say.
		`{"content":[{"type":"text","text":"hello"}]}`:       "",
		`{"content":[{"type":"thinking","thinking":"..."}]}`: "",
		`{"content":[]}`: "",
	}
	for line, want := range cases {
		if got := toolActivity(json.RawMessage(line)); got != want {
			t.Errorf("toolActivity(%.50s) = %q, want %q", line, got, want)
		}
	}
}

func TestDecodeSetsActivityOnAnAssistantLine(t *testing.T) {
	m := decode([]byte(`{"type":"assistant","session_id":"s",
	  "message":{"role":"assistant","content":[
	    {"type":"tool_use","name":"Bash","input":{"command":"ls -la"}}]}}`))
	if m.Activity != "Bash: ls -la" {
		t.Errorf("Activity = %q", m.Activity)
	}
}
