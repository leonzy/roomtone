-- +goose Up
CREATE TABLE player_group_members_new (
 group_id TEXT NOT NULL, player_id TEXT NOT NULL,
 delay_ms INTEGER NOT NULL DEFAULT 0, volume_offset INTEGER NOT NULL DEFAULT 0,
 position INTEGER NOT NULL DEFAULT 0,
 channel_mode TEXT NOT NULL DEFAULT 'stereo' CHECK(channel_mode IN ('stereo','left','right','mono','front_left','front_right','center','lfe','back_left','back_right','back_center','side_left','side_right')),
 PRIMARY KEY(group_id,player_id)
);
INSERT INTO player_group_members_new SELECT group_id,player_id,delay_ms,volume_offset,position,channel_mode FROM player_group_members;
DROP TABLE player_group_members;
ALTER TABLE player_group_members_new RENAME TO player_group_members;
UPDATE configs SET value=json_set(value,'$.supported_formats[#]','dts')
WHERE key='scan_config' AND CASE WHEN json_valid(value) THEN
 CASE WHEN json_type(value,'$.supported_formats')='array' THEN
 NOT EXISTS(SELECT 1 FROM json_each(configs.value,'$.supported_formats') WHERE lower(value)='dts') ELSE 0 END ELSE 0 END;
UPDATE configs SET value=json_set(value,'$.supported_formats[#]','dtshd')
WHERE key='scan_config' AND CASE WHEN json_valid(value) THEN
 CASE WHEN json_type(value,'$.supported_formats')='array' THEN
 NOT EXISTS(SELECT 1 FROM json_each(configs.value,'$.supported_formats') WHERE lower(value)='dtshd') ELSE 0 END ELSE 0 END;

-- +goose Down
CREATE TABLE player_group_members_new (
 group_id TEXT NOT NULL, player_id TEXT NOT NULL,
 delay_ms INTEGER NOT NULL DEFAULT 0, volume_offset INTEGER NOT NULL DEFAULT 0,
 position INTEGER NOT NULL DEFAULT 0,
 channel_mode TEXT NOT NULL DEFAULT 'stereo' CHECK(channel_mode IN ('stereo','left','right','mono')),
 PRIMARY KEY(group_id,player_id)
);
INSERT INTO player_group_members_new SELECT group_id,player_id,delay_ms,volume_offset,position,CASE WHEN channel_mode IN ('stereo','left','right','mono') THEN channel_mode ELSE 'stereo' END FROM player_group_members;
DROP TABLE player_group_members;
ALTER TABLE player_group_members_new RENAME TO player_group_members;
