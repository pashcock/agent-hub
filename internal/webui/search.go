package webui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pashcock/agent-hub/internal/semantic"
	"github.com/pashcock/agent-hub/internal/store"
)

func filterFrom(r *http.Request) store.ConversationFilter {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 100000 {
		page = 100000
	}
	f := store.ConversationFilter{Query: strings.TrimSpace(q.Get("q")), Cwd: q.Get("cwd"), Agent: q.Get("agent"), Model: q.Get("model"), Status: q.Get("status"), Limit: 50, Offset: (page - 1) * 50}
	switch q.Get("period") {
	case "24h":
		f.Since = time.Now().Add(-24 * time.Hour)
	case "7d":
		f.Since = time.Now().Add(-7 * 24 * time.Hour)
	case "30d":
		f.Since = time.Now().Add(-30 * 24 * time.Hour)
	}
	return f
}
func pageLink(q url.Values, page int) string {
	v := url.Values{}
	for k, values := range q {
		v[k] = append([]string{}, values...)
	}
	v.Set("page", strconv.Itoa(page))
	return "/?" + v.Encode()
}
func (u *UI) apiConversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	f := filterFrom(r)
	mode := r.URL.Query().Get("search")
	if mode == "" && u.Semantic != nil {
		mode = "hybrid"
	}
	found, err := u.find(r.Context(), f, mode)
	convs, count := found.Conversations, found.Count
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type item struct {
		Match   *semantic.Hit `json:"match,omitempty"`
		Status  statusJSON    `json:"status"`
		Title   string        `json:"title"`
		Created time.Time     `json:"created_at"`
	}
	out := []item{}
	for _, c := range convs {
		p, err := u.h.Snapshot(r.Context(), c.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var match *semantic.Hit
		if hit, ok := found.Hits[c.ID]; ok {
			match = &hit
		}
		out = append(out, item{match, statusFrom(c, p), u.st.ConversationTitle(r.Context(), c.ID), c.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"search": mode, "notice": found.Notice, "conversations": out, "total": count, "limit": f.Limit, "offset": f.Offset})
}
func (u *UI) actProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	// New operator controls accept same-origin forms only. No new peer tool
	// grants permission to resume an operator-paused provider.
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		http.Error(w, "invalid origin", 403)
		return
	}
	action := r.FormValue("action")
	if action != "pause" && action != "resume" {
		http.Error(w, "invalid action", 400)
		return
	}
	var until time.Time
	if duration := r.FormValue("duration"); action == "pause" && duration != "" {
		d, err := time.ParseDuration(duration)
		if err != nil || d <= 0 {
			http.Error(w, "invalid duration", 400)
			return
		}
		until = time.Now().Add(d)
	}
	if err := u.h.SetProviderPause(r.Context(), r.FormValue("agent"), r.FormValue("reason"), until, action == "pause"); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	back(w, r)
}
