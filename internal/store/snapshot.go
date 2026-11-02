package store

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

func (s *Store) EventCursor(ctx context.Context, id string) (int64, error) {
	var cursor int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM events WHERE conversation_id=?`, id).Scan(&cursor)
	return cursor, err
}
func (s *Store) TurnResult(ctx context.Context, conversation, turn string) (string, error) {
	var result string
	err := s.db.QueryRowContext(ctx, `SELECT text FROM messages WHERE conversation_id=? AND turn_id=? AND role='assistant' ORDER BY created_at DESC,rowid DESC LIMIT 1`, conversation, turn).Scan(&result)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return result, err
}

// History pages are stable rowid cursors, displayed chronologically within
// each page. Zero means latest; adding messages never shifts an older page.
func (s *Store) TurnPage(ctx context.Context, id string, before int64) ([]Turn, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rowid,`+turnCols+` FROM turns WHERE conversation_id=? AND (?=0 OR rowid<?) ORDER BY rowid DESC LIMIT 51`, id, before, before)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Turn{}
	var oldest int64
	var more bool
	for rows.Next() {
		var t Turn
		var row, started int64
		var ended sql.NullInt64
		if err := rows.Scan(&row, &t.ID, &t.ConversationID, &t.Status, &t.Model, &t.Effort, &started, &ended, &t.Error); err != nil {
			return nil, 0, err
		}
		if len(out) == 50 {
			more = true
			break
		}
		t.StartedAt = time.UnixMilli(started)
		if ended.Valid {
			v := time.UnixMilli(ended.Int64)
			t.CompletedAt = &v
		}
		out = append(out, t)
		oldest = row
	}
	if !more {
		oldest = 0
	}
	slices.Reverse(out)
	return out, oldest, rows.Err()
}
func (s *Store) MessagePage(ctx context.Context, id string, before int64) ([]Message, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rowid,id,conversation_id,turn_id,role,text,created_at FROM messages WHERE conversation_id=? AND (?=0 OR rowid<?) ORDER BY rowid DESC LIMIT 101`, id, before, before)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Message{}
	var oldest int64
	var more bool
	for rows.Next() {
		var m Message
		var row, created int64
		if err := rows.Scan(&row, &m.ID, &m.ConversationID, &m.TurnID, &m.Role, &m.Text, &created); err != nil {
			return nil, 0, err
		}
		if len(out) == 100 {
			more = true
			break
		}
		m.CreatedAt = time.UnixMilli(created)
		out = append(out, m)
		oldest = row
	}
	if !more {
		oldest = 0
	}
	slices.Reverse(out)
	return out, oldest, rows.Err()
}
