-- 0026_workflow_critical: mark a workflow as critical (no-opt-out). A critical
-- workflow (e.g. password-reset, security alert) must deliver even when a
-- subscriber has muted that workflow/channel via preferences — subscriber
-- preference suppression is bypassed for critical workflows at delivery time
-- (see internal/preferences.Service.ChannelEnabled). Defaults to 0 (non-critical)
-- so every existing workflow keeps its current (opt-outable) delivery behavior.
-- ADD-only — existing rows are untouched.

ALTER TABLE workflows ADD COLUMN critical INTEGER NOT NULL DEFAULT 0;
