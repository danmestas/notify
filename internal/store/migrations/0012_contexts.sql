-- 0012_contexts: reusable JSON data objects keyed by (org_id, environment_id, type, external_id).
CREATE TABLE IF NOT EXISTS contexts (
    id             TEXT NOT NULL PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    type           TEXT NOT NULL,
    external_id    TEXT NOT NULL,
    data           TEXT NOT NULL DEFAULT '{}',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    UNIQUE (org_id, environment_id, type, external_id)
);
CREATE INDEX IF NOT EXISTS contexts_scope_idx ON contexts (org_id, environment_id);
CREATE INDEX IF NOT EXISTS contexts_type_idx  ON contexts (org_id, environment_id, type);
