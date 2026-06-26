-- 0020_layout_default: mark one layout per environment as the default wrapper.
-- is_default is a SQLite INTEGER (0 or 1). At most one row per environment_id
-- should have is_default=1; the application enforces this via SetDefault
-- (clear-then-set in a transaction) rather than a partial unique index, keeping
-- the schema change backwards-compatible with existing binaries that don't yet
-- call SetDefault.
ALTER TABLE layouts ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0;
