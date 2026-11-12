-- 0056_notification_run_started_at: when the notification's current run id was
-- recorded (#144). A retried trigger reuses its row but starts a fresh run, so
-- the row's created_at does not say how old the run is. Cancelling a
-- transaction uses this age to tell a run dagnats has pruned from one it has
-- not shown yet. Rows that already name a run take their created_at, the
-- closest known value; rows with no run stay empty.
ALTER TABLE notifications ADD COLUMN run_started_at TEXT NOT NULL DEFAULT '';

UPDATE notifications SET run_started_at = created_at WHERE run_id <> '';
