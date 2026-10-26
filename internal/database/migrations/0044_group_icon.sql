-- +goose Up
-- 区域（同步组）图标。空串表示使用客户端默认图标。
-- 图标 key 由服务端白名单校验，见 internal/player/manager.go 的 RenameGroup。
ALTER TABLE player_groups ADD COLUMN icon TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE player_groups DROP COLUMN icon;
