// Package store persists conversations, turns, messages and raw provider
// events in SQLite. It never stores hidden model context or reasoning.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a lookup by id matches no row.
var ErrNotFound = errors.New("store: not found")

// Conversation liveness states. These are exhaustive.
const (
	StatusLive = "live"
	StatusCold = "cold"
	// StatusQueued is a conversation the caller asked to queue because
	// its agent was at capacity. It holds no provider session yet.
	StatusQueued = "queued"
	StatusStale  = "stale"
	StatusClosed = "closed"
)

func validLiveness(s string) bool {
	switch s {
	case StatusLive, StatusCold, StatusQueued, StatusStale, StatusClosed:
		return true
	}
	return false
}

type Conversation struct {
	ID              string
	PeerAgent       string
	CallerKey       string
	ProjectCwd      string
	NativeSessionID string
	Model           string
	Effort          string
	Mode            string
	Sandbox         string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// modernc.org/sqlite serialises writes internally; a single connection
	// keeps WAL and PRAGMA state consistent and avoids "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 5000;" + schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	var hasSandbox int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('conversations') WHERE name='sandbox'`).Scan(&hasSandbox); err != nil {
		db.Close()
		return nil, err
	}
	if hasSandbox == 0 {
		if _, err := db.Exec(`ALTER TABLE conversations ADD COLUMN sandbox TEXT NOT NULL DEFAULT ''`); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

// OpenReadOnly opens an already migrated snapshot without schema or data writes.
func OpenReadOnly(path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA query_only=ON"); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// NewID returns a 128-bit random hex identifier.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("store: entropy unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}

func (s *Store) CreateConversation(ctx context.Context, c Conversation) (string, error) {
	if c.PeerAgent == "" {
		return "", errors.New("store: PeerAgent is required")
	}
	if c.ProjectCwd == "" {
		return "", errors.New("store: ProjectCwd is required")
	}
	if c.ID == "" {
		c.ID = NewID()
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO conversations
            (id, peer_agent, caller_key, project_cwd, native_session_id,
             model, effort, mode, sandbox, status, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.PeerAgent, c.CallerKey, c.ProjectCwd, c.NativeSessionID,
		c.Model, c.Effort, c.Mode, c.Sandbox, StatusLive, now, now)
	if err != nil {
		return "", fmt.Errorf("insert conversation: %w", err)
	}
	return c.ID, nil
}

const conversationCols = `id, peer_agent, caller_key, project_cwd, native_session_id,
                          model, effort, mode, sandbox, status, created_at, updated_at`

func scanConversation(row interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	var created, updated int64
	err := row.Scan(&c.ID, &c.PeerAgent, &c.CallerKey, &c.ProjectCwd,
		&c.NativeSessionID, &c.Model, &c.Effort, &c.Mode, &c.Sandbox, &c.Status,
		&created, &updated)
	if err != nil {
		return Conversation{}, err
	}
	c.CreatedAt = time.UnixMilli(created)
	c.UpdatedAt = time.UnixMilli(updated)
	return c, nil
}

func (s *Store) GetConversation(ctx context.Context, id string) (Conversation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+conversationCols+` FROM conversations WHERE id = ?`, id)
	c, err := scanConversation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("get conversation %s: %w", id, err)
	}
	return c, nil
}

func (s *Store) ListConversations(ctx context.Context) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+conversationCols+` FROM conversations ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetConversationStatus(ctx context.Context, id, status string) error {
	if !validLiveness(status) {
		return fmt.Errorf("store: %q is not a conversation liveness state", status)
	}
	return s.touch(ctx, id,
		`UPDATE conversations SET status = ?, updated_at = ? WHERE id = ?`,
		status)
}

func (s *Store) SetNativeSessionID(ctx context.Context, id, nativeID string) error {
	return s.touch(ctx, id,
		`UPDATE conversations SET native_session_id = ?, updated_at = ? WHERE id = ?`,
		nativeID)
}

func (s *Store) touch(ctx context.Context, id, query string, arg any) error {
	res, err := s.db.ExecContext(ctx, query, arg, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("update conversation %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllCold is called at daemon startup. Every conversation that was
// resident belongs to a process that died with the previous daemon.
// Closed and stale conversations are left untouched.
func (s *Store) MarkAllCold(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET status = ?, updated_at = ? WHERE status = ?`,
		StatusCold, time.Now().UnixMilli(), StatusLive)
	if err != nil {
		return fmt.Errorf("mark all cold: %w", err)
	}
	return nil
}

// Turn states. These are exhaustive. Terminal states are every state
// except TurnQueued and TurnRunning.
const (
	TurnQueued          = "queued"
	TurnRunning         = "running"
	TurnCompleted       = "completed"
	TurnInterrupted     = "interrupted"
	TurnFailed          = "failed"
	TurnNeedsPermission = "needs_permission"
)

func terminalTurnState(s string) bool {
	switch s {
	case TurnCompleted, TurnInterrupted, TurnFailed:
		return true
	}
	return false
}

type Turn struct {
	ID             string
	ConversationID string
	Status         string
	Model          string
	Effort         string
	StartedAt      time.Time
	CompletedAt    *time.Time
	Error          string
}

type Message struct {
	ID             string
	ConversationID string
	TurnID         string
	Role           string
	Text           string
	CreatedAt      time.Time
}

type Event struct {
	Seq            int64
	ConversationID string
	TurnID         string
	Kind           string
	Raw            json.RawMessage
	TS             time.Time
}

// CreateTurn records a turn as running. The caller supplies the id, which
// is the provider's own turn id so Hub state and provider state agree.
func (s *Store) CreateTurn(ctx context.Context, t Turn) (string, error) {
	if t.ConversationID == "" {
		return "", errors.New("store: ConversationID is required")
	}
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.Status == "" {
		t.Status = TurnRunning
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO turns (id, conversation_id, status, model, effort, started_at)
        VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.ConversationID, t.Status, t.Model, t.Effort, time.Now().UnixMilli())
	if err != nil {
		return "", fmt.Errorf("insert turn: %w", err)
	}
	return t.ID, nil
}

const turnCols = `id, conversation_id, status, model, effort, started_at, completed_at, error`

func scanTurn(row interface{ Scan(...any) error }) (Turn, error) {
	var t Turn
	var started int64
	var completed sql.NullInt64
	err := row.Scan(&t.ID, &t.ConversationID, &t.Status, &t.Model, &t.Effort,
		&started, &completed, &t.Error)
	if err != nil {
		return Turn{}, err
	}
	t.StartedAt = time.UnixMilli(started)
	if completed.Valid {
		v := time.UnixMilli(completed.Int64)
		t.CompletedAt = &v
	}
	return t, nil
}

func (s *Store) GetTurn(ctx context.Context, id string) (Turn, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+turnCols+` FROM turns WHERE id = ?`, id)
	t, err := scanTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("get turn %s: %w", id, err)
	}
	return t, nil
}

// LatestTurn returns the most recently started turn of a conversation.
func (s *Store) LatestTurn(ctx context.Context, conversationID string) (Turn, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+turnCols+` FROM turns WHERE conversation_id = ?
         ORDER BY started_at DESC, rowid DESC LIMIT 1`, conversationID)
	t, err := scanTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("latest turn of %s: %w", conversationID, err)
	}
	return t, nil
}

// SetTurnStatus moves a turn to a non-terminal state such as needs_permission.
func (s *Store) SetTurnStatus(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE turns SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("set turn status %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FinishTurn moves a turn to a terminal state and stamps completed_at.
func (s *Store) FinishTurn(ctx context.Context, id, status, errText string) error {
	if !terminalTurnState(status) {
		return fmt.Errorf("store: %q is not a terminal turn state", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE turns SET status = ?, completed_at = ?, error = ? WHERE id = ?`,
		status, time.Now().UnixMilli(), errText, id)
	if err != nil {
		return fmt.Errorf("finish turn %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailRunningTurns is called at daemon startup. Any turn still marked
// running belonged to a provider process that died with the daemon.
func (s *Store) FailRunningTurns(ctx context.Context, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE turns SET status = ?, completed_at = ?, error = ?
         WHERE status IN (?, ?, ?)`,
		TurnFailed, time.Now().UnixMilli(), reason,
		TurnRunning, TurnQueued, TurnNeedsPermission)
	if err != nil {
		return fmt.Errorf("fail running turns: %w", err)
	}
	return nil
}

func (s *Store) AppendMessage(ctx context.Context, conversationID, turnID, role, text string) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO messages (id, conversation_id, turn_id, role, text, created_at)
        VALUES (?, ?, ?, ?, ?, ?)`,
		NewID(), conversationID, turnID, role, text, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("append message: %w", err)
	}
	return nil
}

func (s *Store) ListMessages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, conversation_id, turn_id, role, text, created_at
        FROM messages WHERE conversation_id = ? ORDER BY created_at, rowid`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var created int64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.TurnID, &m.Role, &m.Text, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = time.UnixMilli(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) AppendEvent(ctx context.Context, conversationID, turnID, kind string, raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO events (conversation_id, turn_id, kind, raw_json, ts)
        VALUES (?, ?, ?, ?, ?)`,
		conversationID, turnID, kind, string(raw), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// EventsSince returns up to limit events with seq strictly greater than since.
func (s *Store) EventsSince(ctx context.Context, conversationID string, since int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT seq, conversation_id, turn_id, kind, raw_json, ts
        FROM events WHERE conversation_id = ? AND seq > ?
        ORDER BY seq LIMIT ?`, conversationID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("events since: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var raw string
		var ts int64
		if err := rows.Scan(&e.Seq, &e.ConversationID, &e.TurnID, &e.Kind, &raw, &ts); err != nil {
			return nil, err
		}
		e.Raw = json.RawMessage(raw)
		e.TS = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastEventAt returns when a turn last produced an event. A turn that has
// produced none yet reports when it started, so a peer that goes quiet
// immediately is still measured from a real moment.
func (s *Store) LastEventAt(ctx context.Context, conversationID, turnID string) (time.Time, error) {
	var ts sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(ts) FROM events WHERE conversation_id = ? AND turn_id = ?`,
		conversationID, turnID).Scan(&ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("last event of %s: %w", turnID, err)
	}
	if ts.Valid {
		return time.UnixMilli(ts.Int64), nil
	}
	turn, err := s.GetTurn(ctx, turnID)
	if err != nil {
		return time.Time{}, err
	}
	return turn.StartedAt, nil
}

// QueuedStart is a start waiting for its agent to have room.
type QueuedStart struct {
	ConversationID string
	PeerAgent      string
	RequestJSON    string
	QueuedAt       time.Time
}

// Enqueue records a start to replay once capacity frees.
func (s *Store) Enqueue(ctx context.Context, conversationID, peerAgent, requestJSON string) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT OR REPLACE INTO queued_starts
            (conversation_id, peer_agent, request_json, queued_at)
        VALUES (?, ?, ?, ?)`,
		conversationID, peerAgent, requestJSON, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", conversationID, err)
	}
	return nil
}

// Dequeue removes one entry. It is not an error if there was none.
func (s *Store) Dequeue(ctx context.Context, conversationID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM queued_starts WHERE conversation_id = ?`, conversationID)
	if err != nil {
		return fmt.Errorf("dequeue %s: %w", conversationID, err)
	}
	return nil
}

// ListQueued returns every waiting start, oldest first.
func (s *Store) ListQueued(ctx context.Context) ([]QueuedStart, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT conversation_id, peer_agent, request_json, queued_at
        FROM queued_starts ORDER BY queued_at, rowid`)
	if err != nil {
		return nil, fmt.Errorf("list queued: %w", err)
	}
	defer rows.Close()
	var out []QueuedStart
	for rows.Next() {
		var q QueuedStart
		var at int64
		if err := rows.Scan(&q.ConversationID, &q.PeerAgent, &q.RequestJSON, &at); err != nil {
			return nil, err
		}
		q.QueuedAt = time.UnixMilli(at)
		out = append(out, q)
	}
	return out, rows.Err()
}

// Usage is what one turn, or one conversation, consumed. CostUSD is 0
// when the provider does not report money, which is not the same as free.
type Usage struct {
	Model           string
	InputTokens     int
	OutputTokens    int
	CachedInput     int
	CacheWrite      int
	ReasoningTokens int
	TotalTokens     int
	CostUSD         float64
	DurationMS      int
}

func (u Usage) Empty() bool {
	return u.TotalTokens == 0 && u.InputTokens == 0 && u.OutputTokens == 0 && u.CostUSD == 0
}

// RecordUsage stores the latest reported usage for a turn. Repeated usage
// snapshots replace that row; they must never be summed as new consumption.
func (s *Store) RecordUsage(ctx context.Context, conversationID, turnID string, u Usage) error {
	if turnID == "" {
		return errors.New("store: RecordUsage needs a turn id")
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT OR REPLACE INTO turn_usage
            (turn_id, conversation_id, model, input_tokens, output_tokens,
             cached_input, cache_write, reasoning, total_tokens, cost_usd,
             duration_ms, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		turnID, conversationID, u.Model, u.InputTokens, u.OutputTokens,
		u.CachedInput, u.CacheWrite, u.ReasoningTokens, u.TotalTokens,
		u.CostUSD, u.DurationMS, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("record usage for %s: %w", turnID, err)
	}
	return nil
}

const usageCols = `model, input_tokens, output_tokens, cached_input, cache_write,
                   reasoning, total_tokens, cost_usd, duration_ms`

// TurnUsage returns what one turn consumed. A turn with nothing recorded
// reports an empty Usage and no error.
func (s *Store) TurnUsage(ctx context.Context, turnID string) (Usage, error) {
	var u Usage
	err := s.db.QueryRowContext(ctx,
		`SELECT `+usageCols+` FROM turn_usage WHERE turn_id = ?`, turnID).
		Scan(&u.Model, &u.InputTokens, &u.OutputTokens, &u.CachedInput,
			&u.CacheWrite, &u.ReasoningTokens, &u.TotalTokens, &u.CostUSD, &u.DurationMS)
	if errors.Is(err, sql.ErrNoRows) {
		return Usage{}, nil
	}
	if err != nil {
		return Usage{}, fmt.Errorf("turn usage %s: %w", turnID, err)
	}
	return u, nil
}

// ConversationUsage sums every turn of a conversation.
func (s *Store) ConversationUsage(ctx context.Context, conversationID string) (Usage, error) {
	var u Usage
	err := s.db.QueryRowContext(ctx, `
        SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
               COALESCE(SUM(cached_input),0), COALESCE(SUM(cache_write),0),
               COALESCE(SUM(reasoning),0), COALESCE(SUM(total_tokens),0),
               COALESCE(SUM(cost_usd),0), COALESCE(SUM(duration_ms),0)
        FROM turn_usage WHERE conversation_id = ?`, conversationID).
		Scan(&u.InputTokens, &u.OutputTokens, &u.CachedInput, &u.CacheWrite,
			&u.ReasoningTokens, &u.TotalTokens, &u.CostUSD, &u.DurationMS)
	if err != nil {
		return Usage{}, fmt.Errorf("conversation usage %s: %w", conversationID, err)
	}
	return u, nil
}

// TotalUsage sums everything the hub has ever recorded.
func (s *Store) TotalUsage(ctx context.Context) (Usage, error) {
	var u Usage
	err := s.db.QueryRowContext(ctx, `
        SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
               COALESCE(SUM(cached_input),0), COALESCE(SUM(cache_write),0),
               COALESCE(SUM(reasoning),0), COALESCE(SUM(total_tokens),0),
               COALESCE(SUM(cost_usd),0), COALESCE(SUM(duration_ms),0)
        FROM turn_usage`).
		Scan(&u.InputTokens, &u.OutputTokens, &u.CachedInput, &u.CacheWrite,
			&u.ReasoningTokens, &u.TotalTokens, &u.CostUSD, &u.DurationMS)
	if err != nil {
		return Usage{}, fmt.Errorf("total usage: %w", err)
	}
	return u, nil
}

// ListTurns returns a conversation's turns, oldest first.
func (s *Store) ListTurns(ctx context.Context, conversationID string) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+turnCols+` FROM turns WHERE conversation_id = ?
         ORDER BY started_at, rowid`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatestActivity returns the most recent activity label recorded for a
// turn, or "" when the peer has not said what it is doing.
func (s *Store) LatestActivity(ctx context.Context, conversationID, turnID string) (string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `
        SELECT raw_json FROM events
        WHERE conversation_id = ? AND turn_id = ? AND kind = 'activity'
        ORDER BY seq DESC LIMIT 1`, conversationID, turnID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("latest activity of %s: %w", turnID, err)
	}
	var label string
	if json.Unmarshal([]byte(raw), &label) != nil {
		return "", nil
	}
	return label, nil
}
