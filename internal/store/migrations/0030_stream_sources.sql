-- 0030_stream_sources: inbound webhook-push stream sources. The INBOUND mirror
-- of the outbound integrations catalog: each row is an external producer
-- authorized by a per-source secret to POST event batches into one
-- environment's durable feed, scoped to a single topic prefix.
--
-- secret_hash is the hex SHA-256 of the raw push secret and is the credential
-- lookup key (UNIQUE, globally — the hash IS the credential, resolved without a
-- scope round-trip in the ingest auth path). enc_secret holds the AES-256-GCM
-- ciphertext of the raw secret for reveal/rotation. Limit columns are seeded by
-- the streamsource service (Go-side defaults, kept out of SQL so they stay
-- testable); the DEFAULT 0 here is only a NOT NULL backstop.

CREATE TABLE stream_sources (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    environment_id     TEXT NOT NULL,
    name               TEXT NOT NULL,
    topic              TEXT NOT NULL,
    secret_hash        TEXT NOT NULL UNIQUE,
    enc_secret         BLOB,
    rate_limit_per_min INTEGER NOT NULL DEFAULT 0,
    max_event_bytes    INTEGER NOT NULL DEFAULT 0,
    max_batch_events   INTEGER NOT NULL DEFAULT 0,
    byte_budget        INTEGER NOT NULL DEFAULT 0,
    active             INTEGER NOT NULL DEFAULT 1,
    last_event_at      TEXT NOT NULL DEFAULT '',
    last_error         TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_stream_sources_scope ON stream_sources (org_id, environment_id);
