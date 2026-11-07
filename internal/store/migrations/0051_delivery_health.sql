-- 0051_delivery_health: classified delivery errors and per-integration health
-- (#100). error_class says why a message failed ('auth', 'rate_limited',
-- 'provider_down', 'rejected_message', 'recipient'); empty for every message
-- that did not fail and every row written before this migration.
ALTER TABLE messages ADD COLUMN error_class TEXT NOT NULL DEFAULT '';

-- One row per integration that has had a delivery result or a credential
-- check. state is 'healthy', 'degraded', 'failing' or 'unverified'; since is
-- when it last changed. window_floor is the delivery_results seq the
-- evaluation window starts after: recovering from 'failing' moves it past the
-- failures that caused it. last_error_* hold the latest counted error (class
-- plus the provider's message, which never carries a secret).
CREATE TABLE integration_health (
    integration_id     TEXT PRIMARY KEY REFERENCES integrations(id) ON DELETE CASCADE,
    org_id             TEXT NOT NULL,
    environment_id     TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'unverified',
    since              TEXT NOT NULL,
    window_floor       INTEGER NOT NULL DEFAULT 0,
    last_error_class   TEXT NOT NULL DEFAULT '',
    last_error_message TEXT NOT NULL DEFAULT '',
    last_error_at      TEXT NOT NULL DEFAULT '',
    last_check_at      TEXT NOT NULL DEFAULT '',
    last_check_ok      INTEGER NOT NULL DEFAULT 0,
    updated_at         TEXT NOT NULL
);

-- Every delivery attempt's outcome per integration, in the order it landed:
-- sent and failed messages, and transient failures the engine retries, which
-- write no message. Health counts its window here. Rows older than the
-- window are pruned as new ones arrive. Recipient errors are never written.
CREATE TABLE delivery_results (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    integration_id TEXT NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    ok             INTEGER NOT NULL,
    error_class    TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
);

CREATE INDEX delivery_results_window ON delivery_results (integration_id, created_at);
