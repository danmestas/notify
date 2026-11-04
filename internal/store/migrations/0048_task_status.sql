-- 0048_task_status: the status record of an A2A task (one agent turn), keyed by
-- the user activity id. notes is a JSON array of {text, at} progress lines in
-- arrival order; usage is the final status frame's usage metadata, verbatim.
-- Kept like activities (no purge yet).
CREATE TABLE task_status (
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    task_id        TEXT NOT NULL,
    state          TEXT NOT NULL,
    notes          TEXT NOT NULL DEFAULT '[]',
    usage          TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (org_id, environment_id, task_id)
);
