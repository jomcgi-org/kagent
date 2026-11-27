-- +goose Up

-- A revision keeps settled Session runtimes running for this delay before idle
-- suspension. Zero keeps the earlier suspend-at-once behaviour.
ALTER TABLE runtime_revision
    ADD COLUMN idle_suspend_delay_ms BIGINT NOT NULL DEFAULT 0
        CHECK (idle_suspend_delay_ms >= 0 AND idle_suspend_delay_ms <= 3600000);

-- +goose Down
ALTER TABLE runtime_revision DROP COLUMN idle_suspend_delay_ms;
