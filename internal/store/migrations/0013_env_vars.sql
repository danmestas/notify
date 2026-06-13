-- 0013_env_vars: per-scope environment variables with AES-GCM-encrypted values.
CREATE TABLE IF NOT EXISTS env_vars (
    id             TEXT NOT NULL PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    key            TEXT NOT NULL,
    is_secret      INTEGER NOT NULL DEFAULT 1,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    UNIQUE (org_id, environment_id, key)
);
CREATE TABLE IF NOT EXISTS env_var_values (
    env_var_id     TEXT NOT NULL REFERENCES env_vars(id) ON DELETE CASCADE,
    environment_id TEXT NOT NULL,
    value_enc      BLOB NOT NULL,
    PRIMARY KEY (env_var_id, environment_id)
);
CREATE INDEX IF NOT EXISTS env_vars_scope_idx ON env_vars (org_id, environment_id);
