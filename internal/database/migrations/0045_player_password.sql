-- +goose Up
-- 受密码保护的 AirPlay 音箱的设备密码（A4）。
--
-- 反编译证实 AirPlay 1 密码 = RTSP 上的 HTTP Digest 认证（RFC 2069，
-- 见 internal/player/sink/raop/digest.go），不是 FairPlay。
-- 密码只进服务端（json:"-" 不出 API），API 侧经 PlayerUpdate.Password
-- 单独通道写入；UpsertPlayer 的 ON CONFLICT 不更新该列——重新发现 /
-- 重连不能把用户设置的密码冲掉。
ALTER TABLE players ADD COLUMN password TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE players DROP COLUMN password;
