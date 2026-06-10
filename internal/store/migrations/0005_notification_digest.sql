-- 0005_notification_digest: digest_key marks a notification as the accumulator
-- target for a digest run. Empty string ('') means the notification is not a
-- digest run. Additive column on the existing notifications table.

ALTER TABLE notifications ADD COLUMN digest_key TEXT NOT NULL DEFAULT '';
