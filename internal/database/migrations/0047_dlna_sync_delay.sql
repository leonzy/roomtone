-- +goose Up
-- Estimated extra DLNA audible latency relative to AirPlay. Only mixed/grouped
-- playback uses this value; discovery must not overwrite user calibration.
ALTER TABLE players ADD COLUMN dlna_sync_delay_ms INTEGER NOT NULL DEFAULT 0 CHECK (dlna_sync_delay_ms BETWEEN 0 AND 2000);

-- +goose Down
ALTER TABLE players DROP COLUMN dlna_sync_delay_ms;
