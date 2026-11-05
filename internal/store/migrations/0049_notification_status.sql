-- 0049_notification_status: the trigger now writes a notification row before it
-- starts the row's run (#66), so a row can exist without a run. status says
-- which: 'pending' (written, run not confirmed), 'started' (run id recorded) or
-- 'failed' (the engine refused the start). Every row written before this
-- migration was written after its run started, hence the 'started' default.
ALTER TABLE notifications ADD COLUMN status TEXT NOT NULL DEFAULT 'started';

-- A retried trigger finds the row an earlier attempt wrote for the same
-- (workflow, subscriber, transaction) and reuses it instead of writing another.
CREATE INDEX notifications_by_transaction ON notifications (environment_id, transaction_id);
