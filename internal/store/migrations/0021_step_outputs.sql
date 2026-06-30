-- 0021_step_outputs: per-step output capture for action steps (http_request).
-- An http_request step writes its response (statusCode/body/headers) here keyed
-- by (run_id, step_id) so downstream channel steps can read it as template data
-- ({{steps.<stepId>.statusCode}}) and as a filter-condition source.
--
-- Carries org_id + environment_id per the 0001_init invariant ("every business
-- table carries org_id + environment_id") for compliance deletion and per-tenant
-- analytics, even though ListByRun resolves rows by the globally-unique run_id
-- (the worker has only the opaque run id, mirroring notifications.GetByRunID).
CREATE TABLE step_outputs (
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    run_id         TEXT NOT NULL,
    step_id        TEXT NOT NULL,
    output         TEXT NOT NULL DEFAULT '{}',
    created_at     TEXT NOT NULL,
    PRIMARY KEY (run_id, step_id)
);

CREATE INDEX step_outputs_run_id ON step_outputs(run_id);
