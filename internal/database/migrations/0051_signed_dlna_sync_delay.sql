-- +goose Up
ALTER TABLE players RENAME COLUMN dlna_sync_delay_ms TO dlna_sync_delay_old;
ALTER TABLE players ADD COLUMN dlna_sync_delay_ms INTEGER NOT NULL DEFAULT 0 CHECK (dlna_sync_delay_ms BETWEEN -2000 AND 2000);
UPDATE players SET dlna_sync_delay_ms = dlna_sync_delay_old;
ALTER TABLE players DROP COLUMN dlna_sync_delay_old;

-- +goose Down
ALTER TABLE players RENAME COLUMN dlna_sync_delay_ms TO dlna_sync_delay_old;
ALTER TABLE players ADD COLUMN dlna_sync_delay_ms INTEGER NOT NULL DEFAULT 0 CHECK (dlna_sync_delay_ms BETWEEN 0 AND 2000);
UPDATE players SET dlna_sync_delay_ms = max(0, dlna_sync_delay_old);
ALTER TABLE players DROP COLUMN dlna_sync_delay_old;
