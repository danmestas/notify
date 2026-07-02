-- 0024_builder_sessions_refine: refine-mode support for the AI workflow builder
-- (Phase B4). A builder_session now records whether it CREATEs a new workflow or
-- REFINEs an existing one. On refine, target_slug names the workflow being edited
-- (its trigger identifier) and draft_workflow is seeded with that workflow's
-- current shape so the durable AgentLoop edits it rather than building from empty.
--
-- Existing rows are create-mode with no target (the pre-B4 behavior), which the
-- column defaults preserve.

ALTER TABLE builder_sessions ADD COLUMN mode TEXT NOT NULL DEFAULT 'create';
ALTER TABLE builder_sessions ADD COLUMN target_slug TEXT NOT NULL DEFAULT '';
