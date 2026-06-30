-- 0023_builder_sessions: persistence for the AI workflow builder (Phase B2).
-- A builder_session captures one Guided-tab "generate" run: the prompt, the
-- proposed draft workflow (a WorkflowDraft JSON snapshot), and its lifecycle
-- status (draft -> confirmed/discarded). builder_messages records the per-run
-- reasoning/tool message history collected from the builder's event stream.
--
-- Tenancy: every builder_sessions row carries org_id + environment_id and every
-- read is filtered by both (the 0001 invariant). builder_messages inherits the
-- session's tenancy via session_id and is reached only through a scoped session.

CREATE TABLE builder_sessions (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'draft',
    draft_workflow TEXT NOT NULL DEFAULT '{}',
    prompt         TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE INDEX idx_builder_sessions_scope
    ON builder_sessions (org_id, environment_id, id);

CREATE TABLE builder_messages (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES builder_sessions (id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL DEFAULT '',
    tool_calls TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX idx_builder_messages_session
    ON builder_messages (session_id, id);
