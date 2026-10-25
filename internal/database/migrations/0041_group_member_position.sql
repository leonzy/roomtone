-- +goose Up
-- 成员在组内的显示顺序（决定"主控候选"顺序与前端列表顺序）。
--
-- 用显式 position 而不是依赖插入顺序：SQLite 的 rowid 会随删除/重插变化，
-- 而且 ListAllGroupMembers 需要稳定的 ORDER BY 才能让多端看到同一顺序。
ALTER TABLE player_group_members ADD COLUMN position INTEGER NOT NULL DEFAULT 0;

-- 存量数据按 player_id 铺开，保证升级后顺序确定（而不是全 0 随机）。
UPDATE player_group_members
SET position = (
    SELECT COUNT(*) FROM player_group_members AS m2
    WHERE m2.group_id = player_group_members.group_id
      AND m2.player_id < player_group_members.player_id
);

-- +goose Down
ALTER TABLE player_group_members DROP COLUMN position;
