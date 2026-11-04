-- 0047_a2a_receiver_keys: credentials a live harness presents to answer one
-- agent's turns over the WebSocket receiver. Unlike a client key (which calls
-- notify), a receiver key is bound to a single agent: it can answer that
-- agent's turns and nothing else. Shown once at mint and stored only as a
-- SHA-256 hash; revocation is a soft stamp so the row stays as an audit trail.
CREATE TABLE a2a_receiver_keys (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    agent_id       TEXT NOT NULL,
    name           TEXT NOT NULL,
    secret_hash    TEXT NOT NULL UNIQUE,
    created_at     TEXT NOT NULL DEFAULT '',
    last_used_at   TEXT NOT NULL DEFAULT '',
    revoked_at     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_a2a_receiver_keys_agent ON a2a_receiver_keys (org_id, environment_id, agent_id);
