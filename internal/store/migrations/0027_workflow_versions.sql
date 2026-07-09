-- 0027_workflow_versions: append-only capture of every compiled DAG a publish
-- registers with the engine. authoring.Publish overwrote the live DAG in
-- place with no history, so a bad publish silently degraded in-flight runs
-- (RepublishAll can only log-and-skip a broken row at boot, it cannot undo
-- one). Each row is the exact engine.WorkflowDef bytes RegisterWorkflow was
-- called with, so a bad publish is a ROLLBACK (authoring.Service.Rollback
-- re-registers an earlier row's compiled_def byte-for-byte) rather than a
-- rebuild from scratch.
--
-- Tenancy: a version inherits its workflow's tenancy via workflow_id (the
-- 0001 invariant, reached only through a scope-checked
-- workflow_id -> workflows join); it carries no org_id/environment_id of its
-- own, mirroring builder_checkpoints (migration 0025). ADD-only — existing
-- rows are untouched.
--
-- Retention: capped at 50 versions per workflow. workflow_version_repo.go's
-- Create prunes the oldest rows beyond that cap, in the same transaction as
-- the insert, every time it appends a new version. The newest row is always
-- kept, so RepublishAll's latest-version boot read is unaffected; Rollback is
-- bounded to targeting a version still within the retained window. This
-- supersedes the table's original "grows unbounded, follow-up" note.

CREATE TABLE workflow_versions (
    id                  TEXT PRIMARY KEY,
    workflow_id         TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    seq                 INTEGER NOT NULL,
    name                TEXT NOT NULL,
    trigger_identifier  TEXT NOT NULL,
    compiled_def        TEXT NOT NULL,     -- JSON engine.WorkflowDef, the exact bytes registered
    created_at          TEXT NOT NULL
);

-- UNIQUE so two concurrent publishes on the same workflow can never allocate
-- the same seq (the seq is computed atomically inside the Create INSERT...SELECT,
-- mirroring PushCheckpoint; this index is the backstop that makes a racing
-- duplicate fail loudly rather than silently corrupt the version stack's
-- ordering).
CREATE UNIQUE INDEX idx_workflow_versions_wf ON workflow_versions (workflow_id, seq);
