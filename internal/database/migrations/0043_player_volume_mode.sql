-- +goose Up
-- 每台设备的音量控制方式与硬上限。
--
-- volume_mode:
--   device — 音量直接下发到设备（默认，保持旧行为）
--   fixed  — 固定音量，忽略 SetVolume（适合外接功放 / 线路输出）
--   dsp    — 音量在服务端 DSP 管线里应用
-- max_volume 是用户可见的硬上限，SetVolume 与组音量都必须先钳到该值。
ALTER TABLE players ADD COLUMN volume_mode TEXT NOT NULL DEFAULT 'device'
    CHECK(volume_mode IN ('device','fixed','dsp'));
ALTER TABLE players ADD COLUMN max_volume INTEGER NOT NULL DEFAULT 100
    CHECK(max_volume BETWEEN 0 AND 100);

-- +goose Down
ALTER TABLE players DROP COLUMN max_volume;
ALTER TABLE players DROP COLUMN volume_mode;
