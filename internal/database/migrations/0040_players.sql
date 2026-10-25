-- +goose Up
-- 播放设备、同步组、派对、队列与播放状态。
-- 设计见 docs/architecture_playback.md §6：单机也建只含自己的组，
-- 队列与状态用 owner_kind/owner_id 泛化，派对复用同一套代码。

CREATE TABLE players (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL CHECK(kind IN ('local','raop','dlna')),
    name          TEXT NOT NULL,
    address       TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    capabilities  TEXT NOT NULL DEFAULT '{}',
    volume        INTEGER NOT NULL DEFAULT 0,
    muted         INTEGER NOT NULL DEFAULT 0,
    online        INTEGER NOT NULL DEFAULT 0,
    eq_profile    TEXT NOT NULL DEFAULT '{}',
    takeover_behavior TEXT NOT NULL DEFAULT 'pause'
                  CHECK(takeover_behavior IN ('pause','follow')),
    last_seen_at  DATETIME,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE player_groups (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    master_player_id TEXT NOT NULL DEFAULT '',
    party_id         TEXT,
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE party_sessions (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL DEFAULT '',
    leader_group_id TEXT NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE player_group_members (
    group_id      TEXT NOT NULL,
    player_id     TEXT NOT NULL,
    delay_ms      INTEGER NOT NULL DEFAULT 0,
    volume_offset INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (group_id, player_id)
);

CREATE TABLE playback_queues (
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('group','party')),
    owner_id   TEXT NOT NULL,
    position   INTEGER NOT NULL,
    song_id    INTEGER NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, position)
);

CREATE TABLE playback_states (
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('group','party')),
    owner_id   TEXT NOT NULL,
    song_id    INTEGER NOT NULL DEFAULT 0,
    position   REAL NOT NULL DEFAULT 0,
    state      TEXT NOT NULL DEFAULT 'idle',
    repeat_mode TEXT NOT NULL DEFAULT 'off',
    shuffle    INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (owner_kind, owner_id)
);

CREATE INDEX idx_player_group_members_player ON player_group_members(player_id);
CREATE INDEX idx_playback_queues_owner ON playback_queues(owner_kind, owner_id, position);

-- +goose Down
DROP INDEX IF EXISTS idx_playback_queues_owner;
DROP INDEX IF EXISTS idx_player_group_members_player;
DROP TABLE IF EXISTS playback_states;
DROP TABLE IF EXISTS playback_queues;
DROP TABLE IF EXISTS player_group_members;
DROP TABLE IF EXISTS party_sessions;
DROP TABLE IF EXISTS player_groups;
DROP TABLE IF EXISTS players;
