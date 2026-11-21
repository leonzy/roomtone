-- name: StartListeningEvent :exec
INSERT INTO listening_events (
    play_session_id, song_id, zone_id, context_type, context_key,
    client_id, source, started_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: FinishListeningEvent :exec
UPDATE listening_events
SET ended_at = ?, played_seconds = ?, completion_ratio = ?, updated_at = CURRENT_TIMESTAMP
WHERE play_session_id = ?;

-- name: FinishListeningEventByID :exec
UPDATE listening_events
SET ended_at = ?, played_seconds = ?, completion_ratio = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ?;

-- name: LatestOpenListeningEventID :one
SELECT id FROM listening_events
WHERE song_id = ? AND source = ? AND ended_at IS NULL
ORDER BY started_at DESC, id DESC LIMIT 1;

-- name: RecentListeningSongIDs :many
SELECT DISTINCT song_id FROM listening_events WHERE started_at >= ?;

-- name: GetListeningEventBySession :one
SELECT * FROM listening_events WHERE play_session_id = ?;

-- name: FindRecentOpenListeningEvent :one
SELECT * FROM listening_events
WHERE song_id = ? AND zone_id = ? AND ended_at IS NULL AND started_at >= ?
ORDER BY started_at DESC, id DESC LIMIT 1;
