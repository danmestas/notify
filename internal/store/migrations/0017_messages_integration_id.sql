-- Wave 4 / #42 (BP-13a): add integration_id to messages so the Activity tab
-- can filter by the dispatching integration row without cross-contaminating
-- two integrations that share the same provider_id (e.g. two SMTP rows).
--
-- DEFAULT '' is safe: rows written before this migration carry an empty string
-- which the integrationId filter never matches, so legacy rows are excluded
-- from activity-tab results (acceptable per spec-03 §BP-13).

ALTER TABLE messages ADD COLUMN integration_id TEXT NOT NULL DEFAULT '';

-- Scope-first index covering the Activity tab query:
-- WHERE org_id=? AND environment_id=? AND integration_id=? ORDER BY created_at DESC, id DESC
CREATE INDEX msg_integration ON messages (org_id, environment_id, integration_id, created_at DESC, id DESC);
