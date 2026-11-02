package store

import "context"

// AccountingCoverage distinguishes missing measurements from measured zeroes.
type AccountingCoverage struct {
	Turns              int    `json:"turns"`
	UsageReportedTurns int    `json:"usage_reported_turns"`
	TimedTurns         int    `json:"timed_turns"`
	ExecutionMS        *int64 `json:"execution_ms"`
}

// ConversationCoverage sums observed turn durations, excluding time between turns.
// Only finished timings are counted; current turn progress is in TurnTimings.
func (s *Store) ConversationCoverage(ctx context.Context, id string) (AccountingCoverage, error) {
	var c AccountingCoverage
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(u.turn_id),
 COUNT(CASE WHEN m.turn_id IS NOT NULL AND t.completed_at IS NOT NULL THEN 1 END),
 SUM(CASE WHEN m.turn_id IS NOT NULL AND t.completed_at IS NOT NULL THEN MAX(0,t.completed_at-m.requested_ms) END)
 FROM turns t LEFT JOIN turn_usage u ON u.turn_id=t.id
 LEFT JOIN turn_timing m ON m.turn_id=t.id WHERE t.conversation_id=?`, id).
		Scan(&c.Turns, &c.UsageReportedTurns, &c.TimedTurns, &c.ExecutionMS)
	return c, err
}
