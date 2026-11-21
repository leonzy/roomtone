-- +goose Up
-- +goose StatementBegin
CREATE TABLE listening_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    play_session_id TEXT NOT NULL UNIQUE,
    song_id INTEGER NOT NULL REFERENCES songs(id) ON DELETE CASCADE,
    zone_id TEXT NOT NULL DEFAULT '',
    context_type TEXT NOT NULL DEFAULT '',
    context_key TEXT NOT NULL DEFAULT '',
    client_id TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'other',
    started_at DATETIME NOT NULL,
    ended_at DATETIME,
    played_seconds REAL NOT NULL DEFAULT 0,
    completion_ratio REAL NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_listening_events_started ON listening_events(started_at DESC, id DESC);
CREATE INDEX idx_listening_events_song ON listening_events(song_id, started_at DESC);
CREATE INDEX idx_listening_events_zone ON listening_events(zone_id, started_at DESC);
CREATE INDEX idx_listening_events_context ON listening_events(context_type, context_key, started_at DESC);
CREATE INDEX idx_listening_events_open ON listening_events(song_id, zone_id, ended_at, started_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS listening_events;
-- +goose StatementEnd
