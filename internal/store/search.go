package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ConversationFilter struct {
	IDs    []string // nil means unrestricted; ordered semantic results otherwise.
	Query  string
	Cwd    string
	Agent  string
	Model  string
	Status string
	Since  time.Time
	Limit  int
	Offset int
}

func (f ConversationFilter) where() (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	add := func(s string, a ...any) { clauses = append(clauses, s); args = append(args, a...) }
	if f.IDs != nil {
		if len(f.IDs) == 0 {
			add("0=1")
		} else {
			placeholders := make([]string, len(f.IDs))
			values := make([]any, len(f.IDs))
			for i, id := range f.IDs {
				placeholders[i] = "?"
				values[i] = id
			}
			add("c.id IN ("+strings.Join(placeholders, ",")+")", values...)
		}
	}
	if f.Query != "" {
		q := strings.ToLower(f.Query)
		add(`(instr(lower(c.id),?)>0 OR instr(lower(c.project_cwd),?)>0 OR instr(lower(c.model),?)>0 OR EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND instr(lower(m.text),?)>0))`, q, q, q, q)
	}
	if f.Cwd != "" {
		add(`c.project_cwd=?`, f.Cwd)
	}
	if f.Agent != "" {
		add(`c.peer_agent=?`, f.Agent)
	}
	if f.Model != "" {
		add(`(c.model=? OR EXISTS(SELECT 1 FROM turn_usage u WHERE u.conversation_id=c.id AND u.model=?))`, f.Model, f.Model)
	}
	if f.Status != "" {
		add(`(c.status=? OR (SELECT status FROM turns t WHERE t.conversation_id=c.id ORDER BY started_at DESC,rowid DESC LIMIT 1)=?)`, f.Status, f.Status)
	}
	if !f.Since.IsZero() {
		add(`c.created_at>=?`, f.Since.UnixMilli())
	}
	return strings.Join(clauses, " AND "), args
}
func (s *Store) SearchConversations(ctx context.Context, f ConversationFilter) ([]Conversation, int, error) {
	where, args := f.where()
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations c WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	order := `CASE WHEN c.status='queued' OR (SELECT status FROM turns t WHERE t.conversation_id=c.id ORDER BY started_at DESC,rowid DESC LIMIT 1) IN ('running','needs_permission') THEN 0 ELSE 1 END,created_at DESC,id DESC`
	if len(f.IDs) > 0 {
		parts := []string{"CASE c.id"}
		for i, id := range f.IDs {
			parts = append(parts, fmt.Sprintf("WHEN ? THEN %d", i))
			args = append(args, id)
		}
		order = strings.Join(parts, " ") + " END"
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx, `SELECT `+conversationCols+` FROM conversations c WHERE `+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}
func (s *Store) Workspaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT project_cwd FROM conversations ORDER BY project_cwd`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) ConversationTitle(ctx context.Context, id string) string {
	var text string
	_ = s.db.QueryRowContext(ctx, `SELECT text FROM messages WHERE conversation_id=? AND role='user' ORDER BY created_at,rowid LIMIT 1`, id).Scan(&text)
	text = strings.TrimSpace(strings.ReplaceAll(text, "Do NOT use the superpowers plugin or any of its skills.", ""))
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return id
	}
	runes := []rune(text)
	if len(runes) > 160 {
		text = string(runes[:160]) + "…"
	}
	return text
}

type ModelMetric struct {
	Agent              string
	Model              string
	Turns              int
	Completed          int
	Failed             int
	Measured           int
	FirstTextSamples   int
	ApprovalSamples    int
	AverageTotalMS     *float64
	AverageFirstTextMS *float64
	AverageApprovalMS  *float64
}

// Only completed turns contribute time averages. Counts are not quality scores.
func (s *Store) ModelMetrics(ctx context.Context, f ConversationFilter) ([]ModelMetric, error) {
	where, args := f.where()
	// A conversation may have turns from several models. Restrict the
	// aggregate itself, not just the set of conversations containing it.
	if f.Model != "" {
		where += " AND COALESCE(NULLIF(u.model,''),NULLIF(t.model,''),NULLIF(c.model,''),'unknown')=?"
		args = append(args, f.Model)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.peer_agent,COALESCE(NULLIF(u.model,''),NULLIF(t.model,''),NULLIF(c.model,''),'unknown'),COUNT(*),SUM(t.status='completed'),SUM(t.status='failed'),COUNT(CASE WHEN t.status='completed' THEN m.turn_id END),COUNT(CASE WHEN t.status='completed' THEN m.first_text_ms END),COUNT(CASE WHEN t.status='completed' AND (m.approval_ms>0 OR m.approval_started_ms IS NOT NULL) THEN 1 END),AVG(CASE WHEN t.status='completed' AND m.turn_id IS NOT NULL THEN MAX(0,t.completed_at-m.requested_ms) END),AVG(CASE WHEN t.status='completed' THEN m.first_text_ms-m.prompt_ms END),AVG(CASE WHEN t.status='completed' AND (m.approval_ms>0 OR m.approval_started_ms IS NOT NULL) THEN m.approval_ms+CASE WHEN m.approval_started_ms IS NULL THEN 0 ELSE MAX(0,t.completed_at-m.approval_started_ms) END END)
 FROM turns t JOIN conversations c ON c.id=t.conversation_id LEFT JOIN turn_usage u ON u.turn_id=t.id LEFT JOIN turn_timing m ON m.turn_id=t.id
 WHERE t.completed_at IS NOT NULL AND `+where+` GROUP BY 1,2 ORDER BY 1,2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelMetric{}
	for rows.Next() {
		var m ModelMetric
		if err := rows.Scan(&m.Agent, &m.Model, &m.Turns, &m.Completed, &m.Failed, &m.Measured, &m.FirstTextSamples, &m.ApprovalSamples, &m.AverageTotalMS, &m.AverageFirstTextMS, &m.AverageApprovalMS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
