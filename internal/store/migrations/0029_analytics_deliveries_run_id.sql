-- 0029_analytics_deliveries_run_id: add run_id to analytics_deliveries so the
-- Workflow Runs Trend chart can count DISTINCT workflow runs per day instead
-- of per-day delivery/message rows. A single trigger fans out to one message
-- row per step (channel); those rows share one run_id, so COUNT(DISTINCT
-- run_id) is the honest "runs" metric, not COUNT(*) (QA finding B — the chart
-- was plotting message count and duplicating Delivery Trend under a
-- misleading label). run_id already flows through the delivery-status event
-- (domain.Message.RunID); the projector just wasn't capturing it. ADD-only —
-- existing rows get NULL and are excluded from the runs-per-day query by the
-- "run_id IS NOT NULL AND run_id != ''" guard, same pattern as
-- buildSubscriberQuery's subscriber_id guard.

ALTER TABLE analytics_deliveries ADD COLUMN run_id TEXT;
