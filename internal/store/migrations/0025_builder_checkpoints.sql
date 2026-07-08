-- 0025_builder_checkpoints: per-turn draft snapshots for the AI workflow builder
-- copilot (Slice 1). Each row captures the draft_workflow as it stood BEFORE a
-- follow-up chat turn ran, forming a per-session snapshot stack (ordered by seq).
-- A revert restores builder_sessions.draft_workflow to a snapshot so a turn can be
-- undone. message_id links the snapshot to the builder_messages row (the user turn)
-- that triggered it, for the docked chat panel (Slice 2) to key "revert to here".
--
-- Tenancy: a checkpoint inherits its session's tenancy via session_id and is
-- reached only through a scope-checked session (the 0001 invariant); it carries no
-- org_id/environment_id of its own. ADD-only — existing rows are untouched.

CREATE TABLE builder_checkpoints (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES builder_sessions (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    message_id TEXT NOT NULL DEFAULT '',
    draft_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

-- UNIQUE so two concurrent turns on the same session can never allocate the same
-- seq (the seq is computed atomically inside PushCheckpoint's INSERT...SELECT; this
-- index is the backstop that makes a racing duplicate fail loudly rather than
-- silently corrupt the snapshot stack's ordering).
CREATE UNIQUE INDEX idx_builder_checkpoints_session
    ON builder_checkpoints (session_id, seq);
