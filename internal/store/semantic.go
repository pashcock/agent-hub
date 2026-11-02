package store

import (
	"context"
	"errors"
	"strings"
)

type SearchDocument struct {
	ConversationID, Metadata, Text string
	LiteralSnippet                 string // full-message match, including matches crossing chunk boundaries
}

// SearchDocuments applies project and other filters before exposing text to
// the local encoder. Event payloads and hidden provider context are excluded.
// Hard limits fail explicitly: never silently search only recent history.
func (s *Store) SearchDocuments(ctx context.Context, f ConversationFilter) ([]SearchDocument, error) {
	query := strings.ToLower(f.Query)
	f.Query = ""
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.project_cwd,c.model,COALESCE(m.text,'') FROM conversations c LEFT JOIN messages m ON m.conversation_id=c.id WHERE `+where+` ORDER BY c.id,m.created_at,m.rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SearchDocument{}
	total := 0
	for rows.Next() {
		var id, cwd, model, text string
		if err := rows.Scan(&id, &cwd, &model, &text); err != nil {
			return nil, err
		}
		metadata := id + " " + cwd + " " + model
		literalSnippet := ""
		if query != "" {
			lower := strings.ToLower(text)
			if pos := strings.Index(lower, query); pos >= 0 {
				// Convert the lowercased byte offset to a rune offset; case mapping may
				// change UTF-8 byte widths, but Go simple lowercase preserves rune count.
				start := len([]rune(lower[:pos])) - 80
				if start < 0 {
					start = 0
				}
				chars := []rune(text)
				end := start + 420
				if end > len(chars) {
					end = len(chars)
				}
				literalSnippet = string(chars[start:end])
				if end < len(chars) {
					literalSnippet += "…"
				}
			}
		}
		text = strings.TrimSpace(strings.ReplaceAll(text, "Do NOT use the superpowers plugin or any of its skills.", ""))
		if text == "" {
			text = metadata
		}
		runes := []rune(text)
		total += len(runes)
		if total > 16000000 {
			return nil, errors.New("semantic corpus exceeds 16M characters; narrow workspace or date filters")
		}
		for start := 0; start < len(runes); start += 1000 {
			end := start + 1200
			if end > len(runes) {
				end = len(runes)
			}
			out = append(out, SearchDocument{id, metadata, string(runes[start:end]), literalSnippet})
			if len(out) > 20000 {
				return nil, errors.New("semantic corpus exceeds 20000 chunks; narrow workspace or date filters")
			}
			if end == len(runes) {
				break
			}
		}
	}
	return out, rows.Err()
}
