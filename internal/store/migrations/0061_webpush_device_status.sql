-- 0061_webpush_device_status: what a Web Push device last did (#172), so a
-- client listing a subscriber's devices can show whether each still works.
-- last_success_at is the time of the last delivered push, '' until one is;
-- last_error_class is the provider error class of the last failed push, ''
-- when the last push was delivered or none has been sent.
ALTER TABLE webpush_subscriptions ADD COLUMN last_success_at TEXT NOT NULL DEFAULT '';
ALTER TABLE webpush_subscriptions ADD COLUMN last_error_class TEXT NOT NULL DEFAULT '';
