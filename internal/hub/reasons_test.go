package hub

import (
	"context"
	"strings"
	"testing"

	"github.com/pashcock/agent-hub/internal/store"
)

// A turn that dies for a reason outside the peer's control has to say
// what happened, what it means for the working tree, and what to do next.
// "hub_restart" alone told a caller none of those.
func TestFailureReasonsAreActionable(t *testing.T) {
	for name, reason := range map[string]string{
		"restart":      ReasonHubRestart,
		"stream close": ReasonStreamClosed,
		"closed":       ReasonConversationClosed,
	} {
		if len(reason) < 40 {
			t.Errorf("%s reason is too terse to act on: %q", name, reason)
		}
		// A sentence, not an enum tag. Tool names inside it keep their
		// underscores, so the test looks at shape instead.
		if !strings.Contains(reason, " ") || !strings.HasSuffix(reason, ".") {
			t.Errorf("%s reason is not a sentence: %q", name, reason)
		}
	}
	// The restart case is the one that can leave a half-applied change,
	// so it must say so and name the way forward.
	for _, want := range []string{"restarted", "disk", "peer_send"} {
		if !strings.Contains(ReasonHubRestart, want) {
			t.Errorf("the restart reason never mentions %q: %q", want, ReasonHubRestart)
		}
	}
}

func TestRecoverPutsTheReasonOnTheTurn(t *testing.T) {
	ctx := context.Background()
	h, _, st := newHub(t)
	res, err := h.Start(ctx, StartRequest{Agent: "fake", Task: "x", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	turn, err := st.GetTurn(ctx, res.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != store.TurnFailed {
		t.Fatalf("turn status = %q, want failed", turn.Status)
	}
	if turn.Error != ReasonHubRestart {
		t.Errorf("turn error = %q, want the actionable reason", turn.Error)
	}

	// And the caller sees it through the ordinary poll path.
	pr, err := h.Poll(ctx, res.ConversationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pr.Error, "peer_send") {
		t.Errorf("poll error = %q, want it to name the way forward", pr.Error)
	}
}
