package mcpfacade

import (
	"encoding/json"
	"net/http"
	"time"
)

// Provider control is operator-only; no MCP tool resumes a user pause.
func (f *Facade) RegisterProviderOperator(mux *http.ServeMux, token string) {
	mux.Handle("/operator/provider", RequireToken(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var in struct {
			Agent  string    `json:"agent"`
			Action string    `json:"action"`
			Reason string    `json:"reason"`
			Until  time.Time `json:"until"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		if in.Action != "pause" && in.Action != "resume" {
			http.Error(w, "invalid action", 400)
			return
		}
		if err := f.h.SetProviderPause(r.Context(), in.Agent, in.Reason, in.Until, in.Action == "pause"); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})))
}
