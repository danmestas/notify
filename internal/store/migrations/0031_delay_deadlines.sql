-- 0031_delay_deadlines: persisted fire-at deadline for a relative dynamic
-- delay (delay.dynamic with dynamicKind="relative"), keyed by
-- (run_id, step_id).
--
-- A relative delay's {amount,unit} value describes a DURATION, not a
-- wall-clock instant. Unlike the absolute forms (instant dynamic, scheduled),
-- which recompute the same target on every dispatch, resolving amount+unit
-- against time.Now() on a requeue (deploy, OOM, restart) restarts the full
-- wait from the moment of re-dispatch (#273). The fix resolves the deadline
-- once on first dispatch and reuses it on every subsequent requeue.
--
-- Deliberately its own table, not step_outputs, even though step_outputs has
-- the same (run_id, step_id) key shape:
--   - step_outputs carries its own retention purge, unrelated to (and
--     shorter than) how long a relative delay can run -- amount+unit
--     supports weeks and months. Reusing it would have the deadline purged
--     mid-wait, reintroducing this exact bug in a harder-to-diagnose form.
--   - step_outputs is exposed to downstream steps as template data
--     ({{steps.<stepId>...}}); a deadline is an internal scheduling detail,
--     not workflow-visible data.
--
-- Carries org_id + environment_id per the 0001_init invariant ("every
-- business table carries org_id + environment_id") for compliance deletion,
-- even though the delay worker's own lookups key off (run_id, step_id)
-- directly (it resolves scope from the notification row before writing).
CREATE TABLE delay_deadlines (
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    run_id         TEXT NOT NULL,
    step_id        TEXT NOT NULL,
    deadline       TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    PRIMARY KEY (run_id, step_id)
);

CREATE INDEX delay_deadlines_created_at ON delay_deadlines(created_at);
