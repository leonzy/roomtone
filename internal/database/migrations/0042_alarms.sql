-- +goose Up
-- 服务端闹钟：到点自动在指定组/设备上起播一个歌单并渐强音量。
--
-- 时间用 "HH:MM" 字符串而不是 epoch：闹钟是"每天 07:30"这种本地时刻，
-- 存绝对时间会跟着时区和夏令时错位。days_mask 按 bit0=周一 … bit6=周日，
-- 127 表示每天。
--
-- last_fired_at 存触发到的分钟（"2026-10-26T07:30"），用来做去重：
-- 调度器每 15 秒扫一次，重启后也不会在同一分钟重复起播。
CREATE TABLE IF NOT EXISTS alarms (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL DEFAULT '',
    target_id     TEXT NOT NULL,
    time_of_day   TEXT NOT NULL,
    days_mask     INTEGER NOT NULL DEFAULT 127,
    playlist_id   INTEGER NOT NULL DEFAULT 0,
    volume        INTEGER NOT NULL DEFAULT 50,
    fade_seconds  INTEGER NOT NULL DEFAULT 0,
    enabled       INTEGER NOT NULL DEFAULT 1,
    last_fired_at TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_alarms_enabled ON alarms(enabled, time_of_day);

-- +goose Down
DROP INDEX IF EXISTS idx_alarms_enabled;
DROP TABLE IF EXISTS alarms;
