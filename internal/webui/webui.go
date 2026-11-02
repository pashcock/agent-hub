// Package webui serves a small read-and-act view of the hub over the same
// loopback listener the MCP facade uses.
//
// It is deliberately plain: server-rendered HTML from one template, no
// JavaScript build, no assets on disk. Everything it shows already lives
// in SQLite; this only saves reaching for sqlite3.
//
// It carries no token. The listener is loopback-only, and any process
// running as this user can read hub.db directly, so a token here would
// guard nothing while adding a step to every page load. The token exists
// to keep peers off /mcp, not to keep the operator out.
package webui

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/semantic"
	"github.com/pashcock/agent-hub/internal/store"
)

//go:embed templates/*.html
var files embed.FS

type UI struct {
	Semantic semantic.Scorer
	Preview  bool
	h        *hub.Hub
	st       *store.Store
	tpl      *template.Template
}

func New(h *hub.Hub, st *store.Store) (*UI, error) {
	tpl, err := template.New("").Funcs(funcs()).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("webui templates: %w", err)
	}
	return &UI{h: h, st: st, tpl: tpl}, nil
}

func (u *UI) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", u.overview)
	mux.HandleFunc("/c/", u.conversation)
	mux.HandleFunc("/act/cancel", u.actCancel)
	mux.HandleFunc("/act/approve", u.actApprove)
	mux.HandleFunc("/api/c/", u.api)
	mux.HandleFunc("/api/conversations", u.apiConversations)
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		writeJSON(w, 200, u.h.AgentStatuses())
	})
	mux.HandleFunc("/act/provider", u.actProvider)
}

// ---- view models ----

type convRow struct {
	Match   *semantic.Hit
	Title   string
	Model   string
	Created string
	Timings *store.Timings
	store.Conversation
	TurnStatus string
	Activity   string
	SilentForS int
	Stalled    bool
	Usage      store.Usage
	Age        string
}

type overviewData struct {
	Ranked       bool
	Matches      []convRow
	SearchMode   string
	SearchNotice string
	Preview      bool
	Filter       store.ConversationFilter
	Period       string
	Workspaces   []string
	Count        int
	Page         int
	Previous     string
	Next         string
	Metrics      []store.ModelMetric

	Agents    []hub.AgentStatus
	Live      []convRow
	Done      []convRow
	Queued    []convRow
	Approvals []hub.PendingApproval
	Total     store.Usage
	Now       string
}

type turnRow struct {
	Timings *store.Timings
	store.Turn
	Usage store.Usage
	Took  string
}

type convData struct {
	OlderTurns    int64
	OlderMessages int64
	Older         int64
	Conv          store.Conversation
	Turns         []turnRow
	Messages      []store.Message
	Events        []store.Event
	Usage         store.Usage
	Poll          hub.PollResult
}

// ---- handlers ----

func (u *UI) overview(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	f := filterFrom(r)
	mode := r.URL.Query().Get("search")
	if mode == "" && u.Semantic != nil {
		mode = "hybrid"
	}
	found, err := u.find(ctx, f, mode)
	convs, count := found.Conversations, found.Count
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := overviewData{Ranked: found.Filter.IDs != nil, SearchMode: mode, SearchNotice: found.Notice, Preview: u.Preview, Filter: f, Count: count, Page: f.Offset/f.Limit + 1, Period: r.URL.Query().Get("period"),
		Agents:    u.h.AgentStatuses(),
		Approvals: u.h.PendingApprovals(),
		Now:       time.Now().Format("15:04:05"),
	}
	d.Workspaces, err = u.st.Workspaces(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	d.Metrics, err = u.st.ModelMetrics(ctx, found.Filter)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if d.Page > 1 {
		d.Previous = pageLink(r.URL.Query(), d.Page-1)
	}
	if f.Offset+len(convs) < count {
		d.Next = pageLink(r.URL.Query(), d.Page+1)
	}
	for _, c := range convs {
		row := convRow{Conversation: c, Age: since(c.UpdatedAt), Created: c.CreatedAt.Format("02 Jan 15:04"), Title: u.st.ConversationTitle(ctx, c.ID), Model: c.Model}
		if hit, ok := found.Hits[c.ID]; ok {
			row.Match = &hit
		}
		if usage, err := u.st.ConversationUsage(ctx, c.ID); err == nil {
			row.Usage = usage
			if usage.Model != "" {
				row.Model = usage.Model
			}
		}
		if p, err := u.h.Snapshot(ctx, c.ID); err == nil {
			row.TurnStatus, row.SilentForS, row.Stalled = p.TurnStatus, p.SilentForS, p.Stalled
			row.Activity = p.Activity
			row.Timings = p.Timings
		}
		d.Matches = append(d.Matches, row)
		switch {
		case c.Status == store.StatusQueued:
			d.Queued = append(d.Queued, row)
		case row.TurnStatus == store.TurnRunning || row.TurnStatus == store.TurnNeedsPermission:
			d.Live = append(d.Live, row)
		default:
			d.Done = append(d.Done, row)
		}
	}
	if t, err := u.st.TotalUsage(ctx); err == nil {
		d.Total = t
	}
	u.render(w, "overview.html", d)
}

func (u *UI) conversation(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/c/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	conv, err := u.st.GetConversation(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	d := convData{Conv: conv}
	turnBefore, _ := strconv.ParseInt(r.URL.Query().Get("turn_before"), 10, 64)
	turns, older, err := u.st.TurnPage(ctx, id, turnBefore)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	d.OlderTurns = older
	for _, t := range turns {
		row := turnRow{Turn: t}
		row.Timings, _ = u.st.TurnTimings(ctx, t.ID)
		if usage, err := u.st.TurnUsage(ctx, t.ID); err == nil {
			row.Usage = usage
		}
		if t.CompletedAt != nil {
			row.Took = t.CompletedAt.Sub(t.StartedAt).Round(time.Second).String()
		}
		d.Turns = append(d.Turns, row)
	}
	messageBefore, _ := strconv.ParseInt(r.URL.Query().Get("message_before"), 10, 64)
	d.Messages, d.OlderMessages, err = u.st.MessagePage(ctx, id, messageBefore)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	d.Events, err = u.st.LatestEvents(ctx, id, before, 121)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(d.Events) > 120 {
		d.Events = d.Events[:120]
		d.Older = d.Events[119].Seq
	}

	d.Usage, _ = u.st.ConversationUsage(ctx, id)
	if p, err := u.h.Snapshot(ctx, id); err == nil {
		d.Poll = p
	}
	u.render(w, "conversation.html", d)
}

// actCancel stops a running turn, or takes a queued start out of the
// queue. It always redirects back, so a reload never re-submits.
func (u *UI) actCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.FormValue("conversation")
	if err := u.h.Cancel(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	back(w, r)
}

// actApprove answers a parked permission request, including the
// human-gated classes that peer_approve refuses.
func (u *UI) actApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	decision := r.FormValue("decision")
	if decision != "accept" && decision != "acceptForSession" && decision != "decline" {
		http.Error(w, "decision must be accept, acceptForSession or decline",
			http.StatusBadRequest)
		return
	}
	// The conversation is resolved from the request id, as in the CLI.
	d := adapter.Decision{Verb: decision, Answer: r.FormValue("answer")}
	if err := u.h.ApproveAsOperator(r.Context(), "", r.FormValue("request"), d); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	back(w, r)
}

func back(w http.ResponseWriter, r *http.Request) {
	to := r.FormValue("back")
	if to == "" || !strings.HasPrefix(to, "/") {
		to = "/"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (u *UI) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---- template helpers ----

func funcs() template.FuncMap {
	return template.FuncMap{
		"ms": func(v int64) string { return (time.Duration(v) * time.Millisecond).Round(time.Millisecond).String() },
		"optionalMS": func(v *int64) string {
			if v == nil {
				return "—"
			}
			return (time.Duration(*v) * time.Millisecond).Round(time.Millisecond).String()
		},
		"averageMS": func(v *float64) string {
			if v == nil {
				return "—"
			}
			return (time.Duration(*v) * time.Millisecond).Round(time.Millisecond).String()
		},
		"short": func(s string) string {
			if len(s) > 12 {
				return s[:12]
			}
			return s
		},
		"since":     since,
		"thousands": thousands,
		"cachedInput": func(agent string, u store.Usage) string {
			total := inputWithCache(agent, u)
			if total <= 0 || u.CachedInput > total {
				return thousands(u.CachedInput)
			}
			return fmt.Sprintf("%s (%.1f%%)", thousands(u.CachedInput), 100*float64(u.CachedInput)/float64(total))
		},
		"uncachedInput": func(agent string, u store.Usage) string {
			total := inputWithCache(agent, u)
			if total < u.CachedInput {
				return "—"
			}
			return thousands(total - u.CachedInput)
		},
		"money": func(f float64) string {
			if f == 0 {
				return "—"
			}
			return fmt.Sprintf("$%.4f", f)
		},
		"tokens": func(u store.Usage) string {
			if u.TotalTokens == 0 {
				return "—"
			}
			return thousands(u.TotalTokens)
		},
		"trim": func(s string, n int) string {
			s = strings.ReplaceAll(s, "\n", " ")
			if len(s) <= n {
				return s
			}
			return s[:n] + "…"
		},
		"raw": func(e store.Event) string {
			s := string(e.Raw)
			if len(s) > 400 {
				return s[:400] + "…"
			}
			return s
		},
		"ctx": context.Background,
	}
}

func since(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func thousands(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	s := fmt.Sprint(n)
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, out...), " ")
}

// Claude reports cache reads/writes outside input_tokens; Codex and Grok
// include cache reads in input. Keep raw persisted/provider totals unchanged.
func inputWithCache(agent string, u store.Usage) int {
	if agent == "claude" {
		return u.InputTokens + u.CachedInput + u.CacheWrite
	}
	return u.InputTokens
}
