package store

const schemaSQL = `
CREATE TABLE IF NOT EXISTS provider_pauses (
    agent TEXT PRIMARY KEY,
    reason TEXT NOT NULL,
    until_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS turn_timing (
    turn_id TEXT PRIMARY KEY REFERENCES turns(id) ON DELETE CASCADE,
    requested_ms INTEGER NOT NULL,
    dispatch_ms INTEGER NOT NULL,
    prompt_ms INTEGER NOT NULL,
    accepted_ms INTEGER NOT NULL,
    first_activity_ms INTEGER,
    first_text_ms INTEGER,
    approval_started_ms INTEGER,
    approval_ms INTEGER NOT NULL DEFAULT 0
);

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS conversations (
    id                TEXT PRIMARY KEY,
    peer_agent        TEXT NOT NULL,
    caller_key        TEXT NOT NULL DEFAULT '',
    project_cwd       TEXT NOT NULL,
    native_session_id TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    effort            TEXT NOT NULL DEFAULT '',
    mode              TEXT NOT NULL DEFAULT '',
    sandbox           TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS turns (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    status          TEXT NOT NULL,
    model           TEXT NOT NULL DEFAULT '',
    effort          TEXT NOT NULL DEFAULT '',
    started_at      INTEGER NOT NULL,
    completed_at    INTEGER,
    error           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS turns_by_conversation ON turns(conversation_id, started_at);

CREATE TABLE IF NOT EXISTS messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    turn_id         TEXT NOT NULL DEFAULT '',
    role            TEXT NOT NULL,
    text            TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_by_conversation ON messages(conversation_id, created_at);

CREATE TABLE IF NOT EXISTS events (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    turn_id         TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL,
    raw_json        TEXT NOT NULL,
    ts              INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_by_conversation ON events(conversation_id, seq);

-- A start the caller asked to queue because its agent was at capacity.
-- The whole request is kept so the hub can replay it exactly, including
-- options the conversations table has no column for.
CREATE TABLE IF NOT EXISTS queued_starts (
    conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    peer_agent      TEXT NOT NULL,
    request_json    TEXT NOT NULL,
    queued_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS queued_by_agent ON queued_starts(peer_agent, queued_at);

-- What one turn consumed. Both providers report a turn's usage exactly
-- once, so last write wins is correct and a conversation total is the
-- sum of its turns.
CREATE TABLE IF NOT EXISTS turn_usage (
    turn_id         TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    model           TEXT NOT NULL DEFAULT '',
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cached_input    INTEGER NOT NULL DEFAULT 0,
    cache_write     INTEGER NOT NULL DEFAULT 0,
    reasoning       INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    cost_usd        REAL NOT NULL DEFAULT 0,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS usage_by_conversation ON turn_usage(conversation_id);
`
