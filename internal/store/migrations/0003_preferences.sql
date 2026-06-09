-- 0003_preferences: subscriber channel preferences per (environment, subscriber).
-- type is "subscriberGlobal" (workflow_id empty) or "workflow" (per-workflow
-- override). channels is a JSON object {"in_app":true,"email":false,...}.

CREATE TABLE preferences (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    environment_id  TEXT NOT NULL,
    type            TEXT NOT NULL,
    subscriber_id   TEXT NOT NULL,
    workflow_id     TEXT NOT NULL DEFAULT '',
    channels        TEXT NOT NULL DEFAULT '{}',
    UNIQUE (environment_id, subscriber_id, type, workflow_id)
);

CREATE INDEX pref_subscriber ON preferences (environment_id, subscriber_id);
