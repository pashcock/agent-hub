package store

import (
	"context"
	"database/sql"
	"time"
)

// ProviderPause is operator state, not an inferred subscription quota.
type ProviderPause struct {
	Paused bool       `json:"paused"`
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
}

func (s *Store) ProviderPause(ctx context.Context, agent string) (ProviderPause, error) {
	var reason string
	var until int64
	err := s.db.QueryRowContext(ctx, `SELECT reason, until_ms FROM provider_pauses WHERE agent=?`, agent).Scan(&reason, &until)
	if err == sql.ErrNoRows {
		return ProviderPause{}, nil
	}
	if err != nil {
		return ProviderPause{}, err
	}
	p := ProviderPause{Paused: true, Reason: reason}
	if until != 0 {
		t := time.UnixMilli(until)
		if !t.After(time.Now()) {
			return ProviderPause{}, nil
		}
		p.Until = &t
	}
	return p, nil
}
func (s *Store) SetProviderPause(ctx context.Context, agent, reason string, until time.Time, paused bool) error {
	if !paused {
		_, err := s.db.ExecContext(ctx, `DELETE FROM provider_pauses WHERE agent=?`, agent)
		return err
	}
	var ms int64
	if !until.IsZero() {
		ms = until.UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO provider_pauses(agent,reason,until_ms) VALUES(?,?,?) ON CONFLICT(agent) DO UPDATE SET reason=excluded.reason,until_ms=excluded.until_ms`, agent, reason, ms)
	return err
}

// Timings are observed by the hub; absent measurements remain null.
// Response time starts at prompt submission, not at session creation.
type Timings struct {
	QueueMS         int64  `json:"queue_ms"`
	SetupMS         int64  `json:"setup_ms"`
	SubmitMS        int64  `json:"submit_ms"`
	FirstActivityMS *int64 `json:"first_activity_ms"`
	FirstTextMS     *int64 `json:"first_text_ms"`
	ApprovalMS      int64  `json:"approval_ms"`
	TotalMS         int64  `json:"total_ms"`
}

func (s *Store) BeginTiming(ctx context.Context, id string, requested, dispatch, prompt, accepted time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO turn_timing(turn_id,requested_ms,dispatch_ms,prompt_ms,accepted_ms) VALUES(?,?,?,?,?)`, id, requested.UnixMilli(), dispatch.UnixMilli(), prompt.UnixMilli(), accepted.UnixMilli())
	return err
}
func (s *Store) ObserveTiming(ctx context.Context, id, kind string, at time.Time) error {
	var query string
	switch kind {
	case "activity", "message_delta":
		query = `UPDATE turn_timing SET first_activity_ms=COALESCE(first_activity_ms,?) WHERE turn_id=?`
	case "approval_requested":
		query = `UPDATE turn_timing SET approval_started_ms=COALESCE(approval_started_ms,?) WHERE turn_id=?`
	default:
		return nil
	}
	if _, err := s.db.ExecContext(ctx, query, at.UnixMilli(), id); err != nil {
		return err
	}
	if kind == "message_delta" {
		_, err := s.db.ExecContext(ctx, `UPDATE turn_timing SET first_text_ms=COALESCE(first_text_ms,?) WHERE turn_id=?`, at.UnixMilli(), id)
		return err
	}
	return nil
}
func (s *Store) EndApprovalTiming(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE turn_timing SET approval_ms=approval_ms+MAX(0,?-COALESCE(approval_started_ms,?)),approval_started_ms=NULL WHERE turn_id=?`, at.UnixMilli(), at.UnixMilli(), id)
	return err
}
func (s *Store) TurnTimings(ctx context.Context, id string) (*Timings, error) {
	var req, dispatch, prompt, accepted, approval int64
	var activity, text, parked, end sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT requested_ms,dispatch_ms,prompt_ms,accepted_ms,first_activity_ms,first_text_ms,approval_started_ms,approval_ms,t.completed_at FROM turn_timing m JOIN turns t ON t.id=m.turn_id WHERE turn_id=?`, id).Scan(&req, &dispatch, &prompt, &accepted, &activity, &text, &parked, &approval, &end)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	finish := time.Now().UnixMilli()
	if end.Valid {
		finish = end.Int64
	}
	t := &Timings{QueueMS: max(0, dispatch-req), SetupMS: max(0, prompt-dispatch), SubmitMS: max(0, accepted-prompt), TotalMS: max(0, finish-req), ApprovalMS: approval}
	if activity.Valid {
		v := max(0, activity.Int64-prompt)
		t.FirstActivityMS = &v
	}
	if text.Valid {
		v := max(0, text.Int64-prompt)
		t.FirstTextMS = &v
	}
	if parked.Valid {
		t.ApprovalMS += max(0, finish-parked.Int64)
	}
	return t, nil
}

func (s *Store) LatestEvents(ctx context.Context, id string, before int64, limit int) ([]Event, error) {
	if limit < 1 || limit > 400 {
		limit = 120
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,conversation_id,turn_id,kind,raw_json,ts FROM events WHERE conversation_id=? AND (?=0 OR seq<?) ORDER BY seq DESC LIMIT ?`, id, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var raw string
		var ts int64
		if err := rows.Scan(&e.Seq, &e.ConversationID, &e.TurnID, &e.Kind, &raw, &ts); err != nil {
			return nil, err
		}
		e.Raw = []byte(raw)
		e.TS = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
