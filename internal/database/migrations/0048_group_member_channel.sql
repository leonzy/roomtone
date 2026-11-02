-- +goose Up
ALTER TABLE player_group_members ADD COLUMN channel_mode TEXT NOT NULL DEFAULT 'stereo' CHECK (channel_mode IN ('stereo','left','right','mono'));

-- +goose Down
ALTER TABLE player_group_members DROP COLUMN channel_mode;
