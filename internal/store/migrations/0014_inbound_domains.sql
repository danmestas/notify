-- 0014_inbound_domains: domains for inbound email reception.
CREATE TABLE IF NOT EXISTS inbound_domains (
    id                  TEXT NOT NULL PRIMARY KEY,
    org_id              TEXT NOT NULL,
    environment_id      TEXT NOT NULL,
    name                TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending',
    mx_configured       INTEGER NOT NULL DEFAULT 0,
    expected_mx_host    TEXT NOT NULL DEFAULT '',
    expected_mx_priority INTEGER NOT NULL DEFAULT 10,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    UNIQUE (org_id, environment_id, name)
);
CREATE INDEX IF NOT EXISTS inbound_domains_scope_idx ON inbound_domains (org_id, environment_id);
