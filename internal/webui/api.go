package webui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

// The JSON view allows waiting outside a model-driven polling loop.
// /api/c/<id> is a snapshot; /api/c/<id>/wait holds the request until the
// turn stops or needs input. Background completion notification depends
// on the caller's harness; HTTP alone does not wake an idle agent.
// These read endpoints share the web UI's loopback-only access policy.
const (
	defaultWaitS = 300
	maxWaitS     = 900
	// waitPoll is how often the wait loop looks. The read is a local
	// SQLite query, and nobody is billed for it.
	waitPoll = 300 * time.Millisecond
)

// waitBudget clamps a requested timeout into what the hub will hold a
// connection open for. A caller that wants longer reconnects, which also
// gives it a chance to notice the hub restarted.
func waitBudget(asked int) int {
	switch {
	case asked <= 0:
		return defaultWaitS
	case asked > maxWaitS:
		return maxWaitS
	default:
		return asked
	}
}

type usageJSON = hub.Consumption

type approvalJSON struct {
	ID         string `json:"id"`
	Class      string `json:"class"`
	Command    string `json:"command,omitempty"`
	Reason     string `json:"reason,omitempty"`
	HumanGated bool   `json:"human_gated"`
}

type statusJSON struct {
	Conversation   *hub.ConversationAccounting `json:"conversation,omitempty"`
	Timings        *store.Timings              `json:"timings,omitempty"`
	ConversationID string                      `json:"conversation_id"`
	Agent          string                      `json:"agent"`
	Mode           string                      `json:"mode,omitempty"`
	Cwd            string                      `json:"cwd,omitempty"`
	Liveness       string                      `json:"liveness"`
	TurnID         string                      `json:"turn_id,omitempty"`
	TurnStatus     string                      `json:"turn_status,omitempty"`
	// Done is the field a waiting loop tests: the peer is not working on
	// anything, for any reason — finished, failed, cancelled, or stuck on
	// a permission request that somebody has to answer.
	Done       bool   `json:"done"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	Activity   string `json:"activity,omitempty"`
	SilentForS int    `json:"silent_for_s"`
	Stalled    bool   `json:"stalled"`
	Cursor     int64  `json:"cursor"`
	// TimedOut is set only by /wait, where false and true mean different
	// things. A snapshot leaves it out.
	TimedOut        *bool         `json:"timed_out,omitempty"`
	Usage           *usageJSON    `json:"usage,omitempty"`
	PendingApproval *approvalJSON `json:"pending_approval,omitempty"`
}

// turnIsOver reports whether anything is still running. A conversation
// with no turn at all counts as over: there is nothing to wait for.
func turnIsOver(status string) bool {
	switch status {
	case store.TurnRunning, store.TurnQueued:
		return false
	}
	return true
}

func statusFrom(conv store.Conversation, res hub.PollResult) statusJSON {
	out := statusJSON{Timings: res.Timings, Conversation: res.Conversation,
		ConversationID: res.ConversationID,
		Agent:          conv.PeerAgent,
		Mode:           conv.Mode,
		Cwd:            conv.ProjectCwd,
		Liveness:       res.Liveness,
		TurnID:         res.TurnID,
		TurnStatus:     res.TurnStatus,
		Done:           turnIsOver(res.TurnStatus),
		Result:         res.Result,
		Error:          res.Error,
		Activity:       res.Activity,
		SilentForS:     res.SilentForS,
		Stalled:        res.Stalled,
		Cursor:         res.Cursor,
	}
	if u := res.Usage; u != nil {
		v := hub.ConsumptionOf(conv.PeerAgent, *u)
		out.Usage = &v
	}
	if p := res.PendingApproval; p != nil {
		out.PendingApproval = &approvalJSON{
			ID: p.ID, Class: p.Class, Command: p.Command,
			Reason: p.Reason, HumanGated: p.HumanGated,
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// api serves /api/c/<id> and /api/c/<id>/wait.
func (u *UI) api(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed,
			map[string]string{"error": "this endpoint only reads; use the web view or the MCP tools to act"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/c/")
	id, wait := path, false
	if rest, ok := strings.CutSuffix(path, "/wait"); ok {
		id, wait = rest, true
	}
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such conversation"})
		return
	}

	ctx := r.Context()
	conv, err := u.st.GetConversation(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	res, err := u.h.Snapshot(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if !wait {
		writeJSON(w, http.StatusOK, statusFrom(conv, res))
		return
	}

	budget, _ := strconv.Atoi(r.URL.Query().Get("timeout_s"))
	deadline := time.Now().Add(time.Duration(waitBudget(budget)) * time.Second)
	timedOut := false
	for !turnIsOver(res.TurnStatus) {
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
		select {
		case <-ctx.Done():
			return // The caller hung up; nothing to report to.
		case <-time.After(waitPoll):
		}
		next, err := u.h.Snapshot(ctx, id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		res = next
	}
	out := statusFrom(conv, res)
	out.TimedOut = &timedOut
	writeJSON(w, http.StatusOK, out)
}
