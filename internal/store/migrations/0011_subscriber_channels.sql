-- 0011_subscriber_channels: per-subscriber channel addressing (push topics,
-- chat webhooks) per 0001 §3. credentials is a JSON document (e.g.
-- {"topic":"agent-alerts"} for ntfy); server-side secrets stay in
-- integrations.enc_credentials. Every business table carries org_id +
-- environment_id; the dedup key is (environment, subscriber, provider).

CREATE TABLE subscriber_channels (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    environment_id  TEXT NOT NULL,
    subscriber_id   TEXT NOT NULL,
    provider_id     TEXT NOT NULL,
    type            TEXT NOT NULL DEFAULT '',
    credentials     TEXT,
    UNIQUE (environment_id, subscriber_id, provider_id)
);

CREATE INDEX subscriber_channels_lookup ON subscriber_channels (environment_id, subscriber_id, type);
