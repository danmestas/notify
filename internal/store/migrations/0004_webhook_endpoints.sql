-- 0004_webhook_endpoints: outbound delivery-status callback URLs per
-- (environment). active=1 endpoints receive delivery events; inactive rows
-- are retained but skipped by ListActiveByEnv.

CREATE TABLE webhook_endpoints (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    environment_id  TEXT NOT NULL,
    url             TEXT NOT NULL,
    active          INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX webhook_endpoint_env_active ON webhook_endpoints (environment_id, active);
