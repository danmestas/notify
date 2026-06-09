-- 0001_init: schema_migrations + the in-app-path tables only (Ousterhout §D).
-- ULID TEXT primary keys; every business table carries org_id + environment_id.

CREATE TABLE schema_migrations (
    filename   TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE organizations (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE environments (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    name       TEXT NOT NULL,
    identifier TEXT NOT NULL,
    type       TEXT NOT NULL DEFAULT '',
    bridge_url TEXT NOT NULL DEFAULT '',
    parent_id  TEXT NOT NULL DEFAULT '',
    UNIQUE (org_id, identifier)
);

CREATE TABLE api_keys (
    hash           TEXT PRIMARY KEY,
    environment_id TEXT NOT NULL,
    org_id         TEXT NOT NULL,
    enc_key        BLOB,
    user_id        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE subscribers (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    subscriber_id  TEXT NOT NULL,
    email          TEXT NOT NULL DEFAULT '',
    phone          TEXT NOT NULL DEFAULT '',
    first_name     TEXT NOT NULL DEFAULT '',
    last_name      TEXT NOT NULL DEFAULT '',
    locale         TEXT NOT NULL DEFAULT '',
    timezone       TEXT NOT NULL DEFAULT '',
    data           TEXT,
    deleted        INTEGER NOT NULL DEFAULT 0,
    UNIQUE (environment_id, subscriber_id)
);

CREATE TABLE workflows (
    id                   TEXT PRIMARY KEY,
    org_id               TEXT NOT NULL,
    environment_id       TEXT NOT NULL,
    name                 TEXT NOT NULL,
    trigger_identifier   TEXT NOT NULL,
    active               INTEGER NOT NULL DEFAULT 0,
    origin               TEXT NOT NULL DEFAULT '',
    steps                TEXT,
    payload_schema       TEXT,
    compiled_dag_version TEXT NOT NULL DEFAULT '',
    UNIQUE (environment_id, trigger_identifier)
);

CREATE TABLE notifications (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    workflow_id    TEXT NOT NULL DEFAULT '',
    subscriber_id  TEXT NOT NULL DEFAULT '',
    transaction_id TEXT NOT NULL DEFAULT '',
    run_id         TEXT NOT NULL DEFAULT '',
    payload        TEXT,
    created_at     TEXT NOT NULL
);

CREATE TABLE messages (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    environment_id  TEXT NOT NULL,
    workflow_id     TEXT NOT NULL DEFAULT '',
    notification_id TEXT NOT NULL DEFAULT '',
    run_id          TEXT NOT NULL DEFAULT '',
    step_id         TEXT NOT NULL DEFAULT '',
    subscriber_id   TEXT NOT NULL DEFAULT '',
    channel         TEXT NOT NULL DEFAULT '',
    provider_id     TEXT NOT NULL DEFAULT '',
    content         TEXT NOT NULL DEFAULT '',
    subject         TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT '',
    seen            INTEGER NOT NULL DEFAULT 0,
    read            INTEGER NOT NULL DEFAULT 0,
    archived        INTEGER NOT NULL DEFAULT 0,
    transaction_id  TEXT NOT NULL DEFAULT '',
    error_text      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL
);

CREATE INDEX msg_inbox ON messages (environment_id, subscriber_id, channel, created_at);
