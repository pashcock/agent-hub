package mcpfacade

import (
	"encoding/json"
	"net/http"

	"github.com/pashcock/agent-hub/internal/adapter"
)

// RegisterOperator adds the local-only endpoints the agent-hub CLI uses to
// answer approvals that peer_approve refuses. They are reachable only over
// the loopback listener the daemon binds.
func (f *Facade) RegisterOperator(mux *http.ServeMux) {
	mux.HandleFunc("/operator/approvals", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pending := f.h.PendingApprovals()
		out := struct {
			Approvals []approvalOut `json:"approvals"`
		}{Approvals: make([]approvalOut, 0, len(pending))}
		for _, p := range pending {
			out.Approvals = append(out.Approvals, approvalOut{
				ID: p.ID, Class: p.Class, Command: p.Command, Paths: p.Paths,
				Cwd: p.Cwd, Reason: p.Reason, HumanGated: p.HumanGated,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("/operator/approve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var in struct {
			ConversationID string `json:"conversation_id"`
			RequestID      string `json:"request_id"`
			Decision       string `json:"decision"`
			Answer         string `json:"answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := f.h.ApproveAsOperator(r.Context(), in.ConversationID, in.RequestID,
			adapter.Decision{Verb: in.Decision, Answer: in.Answer}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
