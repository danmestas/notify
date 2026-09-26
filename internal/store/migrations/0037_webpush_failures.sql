-- 0037_webpush_failures: consecutive permanent refusals per Web Push
-- subscription (#360). A subscription the push service refuses with a
-- non-gone 4xx three times in a row is retired, like a 404/410 one; any
-- successful delivery resets the count to zero.
ALTER TABLE webpush_subscriptions ADD COLUMN failures INTEGER NOT NULL DEFAULT 0;
