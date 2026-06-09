-- 0006_analytics_deliveries: append-only sink for the analytics projector that
-- drains delivery events. One row per (channel, status) delivery observation,
-- scope-keyed by (org_id, environment_id). Query side groups by channel+status.

CREATE TABLE analytics_deliveries (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    environment_id  TEXT NOT NULL,
    channel         TEXT NOT NULL,
    status          TEXT NOT NULL,
    subscriber_id   TEXT,
    workflow_id     TEXT,
    transaction_id  TEXT,
    created_at      TEXT NOT NULL
);

CREATE INDEX analytics_delivery_env_channel_status
    ON analytics_deliveries (environment_id, channel, status);
