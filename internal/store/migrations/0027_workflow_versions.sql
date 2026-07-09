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
-- Retention: this table grows one row per publish with no pruning policy —
-- deliberately deferred; flagged as a follow-up.

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
