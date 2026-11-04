-- 0046_a2a_client_keys: credentials for machine callers of the A2A channel.
-- A client key is per environment, named, optionally bound to one subscriber
-- (the caller then speaks as that person), shown once at mint and stored only
-- as a SHA-256 hash -- there is no ciphertext column, so the secret cannot be
-- recovered. Revocation is a soft stamp so the row stays as an audit trail.
CREATE TABLE a2a_client_keys (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    name           TEXT NOT NULL,
    subscriber_id  TEXT NOT NULL DEFAULT '',
    secret_hash    TEXT NOT NULL UNIQUE,
    created_at     TEXT NOT NULL DEFAULT '',
    last_used_at   TEXT NOT NULL DEFAULT '',
    revoked_at     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_a2a_client_keys_scope ON a2a_client_keys (org_id, environment_id);
