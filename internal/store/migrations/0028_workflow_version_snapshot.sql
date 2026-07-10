-- 0028_workflow_version_snapshot: extend workflow_versions into a full
-- snapshot of the editable authoring row (workflows.steps/payload_schema/
-- critical), not just the compiled engine DAG (migration 0027 already
-- captures name/trigger_identifier/compiled_def).
--
-- Prior to this migration, authoring.Rollback re-registered an earlier
-- version's compiled_def with the engine but never touched workflows.steps —
-- and delivery reads step CONTENT from that live row at send time
-- (internal/delivery/rebuild.go), not from the compiled def. A rollback
-- therefore ran a hybrid: the rolled-back version's DAG TOPOLOGY (step ids,
-- ordering) against the CURRENT row's CONTENT, which errors outright at
-- delivery when a later edit renamed or removed a step id the older def
-- still references. This is a correctness bug, not just an honesty gap in
-- the confirm copy.
--
-- With steps/payload_schema/critical captured alongside compiled_def,
-- Rollback can restore the whole editable row atomically with re-registering
-- the DAG, so delivery and the engine agree again post-rollback.
--
-- Nullable/defaulted so every pre-existing workflow_versions row (captured
-- before this migration) stays valid: steps and payload_schema default to ''
-- (authoring.Service.Rollback treats an empty steps column as "this version
-- predates full-row snapshots" and falls back to the old compiled_def-only
-- restore instead of blanking the live row). critical defaults to 0.
-- ADD-only — existing rows are untouched.

ALTER TABLE workflow_versions ADD COLUMN steps TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_versions ADD COLUMN payload_schema TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_versions ADD COLUMN critical INTEGER NOT NULL DEFAULT 0;
