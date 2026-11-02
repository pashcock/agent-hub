package codex

import "testing"

func TestActivityLabelCoversTheItemsThatSayWhatAPeerIsDoing(t *testing.T) {
	cases := []struct {
		item TurnItem
		want string
	}{
		{TurnItem{Type: "commandExecution", Command: "go test ./...\nmore"}, "running go test ./..."},
		{TurnItem{Type: "commandExecution"}, "running a command"},
		{TurnItem{Type: "fileChange"}, "editing files"},
		{TurnItem{Type: "mcpToolCall", Server: "node_repl"}, "calling node_repl"},
		{TurnItem{Type: "webSearch", Query: "golang mutex"}, "searching the web for golang mutex"},
		{TurnItem{Type: "imageView", Path: "/tmp/a.png"}, "viewing /tmp/a.png"},
		{TurnItem{Type: "reasoning"}, "thinking"},
		{TurnItem{Type: "plan"}, "planning"},
		{TurnItem{Type: "contextCompaction"}, "compacting its context"},
		{TurnItem{Type: "subAgentActivity", Kind: "started"}, "subagent: started"},
		// Items that say nothing useful must stay silent rather than
		// pushing noise into every poll.
		{TurnItem{Type: "agentMessage", Text: "hello"}, ""},
		{TurnItem{Type: "userMessage"}, ""},
		{TurnItem{Type: "somethingNew"}, ""},
	}
	for _, c := range cases {
		if got := activityLabel(c.item); got != c.want {
			t.Errorf("activityLabel(%s) = %q, want %q", c.item.Type, got, c.want)
		}
	}
}

func TestFirstLineTrimsHard(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	got := firstLine(long)
	if len([]rune(got)) > 91 {
		t.Errorf("firstLine returned %d runes; a poll line must stay short", len([]rune(got)))
	}
}
