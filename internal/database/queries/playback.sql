-- name: ListPlayers :many
SELECT * FROM players ORDER BY created_at, id;

-- name: GetPlayer :one
SELECT * FROM players WHERE id = ?;

-- name: UpsertPlayer :exec
INSERT INTO players (id, kind, name, address, model, capabilities, volume, muted, online, eq_profile, takeover_behavior, volume_mode, max_volume, password, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    kind = excluded.kind,
    name = excluded.name,
    address = excluded.address,
    model = excluded.model,
    capabilities = excluded.capabilities,
    online = excluded.online,
    last_seen_at = excluded.last_seen_at;

-- name: UpdatePlayerConfig :exec
UPDATE players SET name = ?, takeover_behavior = ?, volume_mode = ?, max_volume = ?, password = ? WHERE id = ?;

-- name: UpdatePlayerVolume :exec
UPDATE players SET volume = ?, muted = ? WHERE id = ?;

-- name: UpdatePlayerEQ :exec
UPDATE players SET eq_profile = ? WHERE id = ?;

-- name: UpdatePlayerOnline :exec
UPDATE players SET online = ?, last_seen_at = ? WHERE id = ?;

-- name: DeletePlayer :execrows
DELETE FROM players WHERE id = ?;

-- name: ListGroups :many
SELECT * FROM player_groups ORDER BY created_at, id;

-- name: GetGroup :one
SELECT * FROM player_groups WHERE id = ?;

-- name: InsertGroup :exec
INSERT INTO player_groups (id, name, master_player_id, party_id) VALUES (?, ?, ?, ?);

-- name: UpdateGroup :exec
UPDATE player_groups SET name = ?, master_player_id = ?, party_id = ? WHERE id = ?;

-- name: UpdateGroupIdentity :execrows
UPDATE player_groups SET name = ?, icon = ? WHERE id = ?;

-- name: DeleteGroup :execrows
DELETE FROM player_groups WHERE id = ?;

-- name: ListAllGroupMembers :many
SELECT * FROM player_group_members ORDER BY group_id, position, player_id;

-- name: ListGroupMembers :many
SELECT * FROM player_group_members WHERE group_id = ? ORDER BY position, player_id;

-- name: InsertGroupMember :exec
INSERT INTO player_group_members (group_id, player_id, delay_ms, volume_offset, position)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(group_id, player_id) DO UPDATE SET
    delay_ms = excluded.delay_ms,
    volume_offset = excluded.volume_offset;

-- name: UpdateGroupMemberPosition :exec
UPDATE player_group_members SET position = ? WHERE group_id = ? AND player_id = ?;

-- name: DeleteGroupMember :execrows
DELETE FROM player_group_members WHERE group_id = ? AND player_id = ?;

-- name: DeleteGroupMembers :exec
DELETE FROM player_group_members WHERE group_id = ?;

-- name: DeleteGroupsOfPlayer :exec
DELETE FROM player_group_members WHERE player_id = ?;

-- name: InsertParty :exec
INSERT INTO party_sessions (id, name, leader_group_id) VALUES (?, ?, ?);

-- name: GetParty :one
SELECT * FROM party_sessions WHERE id = ?;

-- name: ListParties :many
SELECT * FROM party_sessions ORDER BY created_at, id;

-- name: UpdatePartyLeader :exec
UPDATE party_sessions SET leader_group_id = ? WHERE id = ?;

-- name: DeleteParty :execrows
DELETE FROM party_sessions WHERE id = ?;

-- name: ListQueue :many
SELECT song_id FROM playback_queues
WHERE owner_kind = ? AND owner_id = ?
ORDER BY position;

-- name: DeleteQueue :exec
DELETE FROM playback_queues WHERE owner_kind = ? AND owner_id = ?;

-- name: InsertQueueItem :exec
INSERT INTO playback_queues (owner_kind, owner_id, position, song_id) VALUES (?, ?, ?, ?);

-- name: GetState :one
SELECT * FROM playback_states WHERE owner_kind = ? AND owner_id = ?;

-- name: UpsertState :exec
INSERT INTO playback_states (owner_kind, owner_id, song_id, position, state, repeat_mode, shuffle, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(owner_kind, owner_id) DO UPDATE SET
    song_id = excluded.song_id,
    position = excluded.position,
    state = excluded.state,
    repeat_mode = excluded.repeat_mode,
    shuffle = excluded.shuffle,
    updated_at = excluded.updated_at;

-- name: DeleteState :exec
DELETE FROM playback_states WHERE owner_kind = ? AND owner_id = ?;
