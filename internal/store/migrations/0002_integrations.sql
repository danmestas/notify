-- 0002_integrations: provider credentials per (environment, channel).
-- enc_credentials holds the encrypted provider secret blob; configurations is
-- a JSON document. Every business table carries org_id + environment_id.

CREATE TABLE integrations (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL,
    environment_id   TEXT NOT NULL,
    provider_id      TEXT NOT NULL,
    channel          TEXT NOT NULL,
    enc_credentials  BLOB,
    configurations   TEXT,
    active           INTEGER NOT NULL DEFAULT 1,
    primary_provider INTEGER NOT NULL DEFAULT 0,
    priority         INTEGER NOT NULL DEFAULT 0,
    identifier       TEXT,
    UNIQUE (environment_id, identifier)
);

CREATE INDEX integ_active ON integrations (environment_id, channel, active);
