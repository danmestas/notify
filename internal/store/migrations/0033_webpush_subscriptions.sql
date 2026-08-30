-- 0033_webpush_subscriptions: per-device Web Push subscriptions.
--
-- The browser PushSubscription (endpoint + keys.p256dh + keys.auth) is
-- per-device, unlike every other push provider's single per-subscriber
-- token, so it gets its own table instead of a subscriber_channels row: a
-- subscriber with three browser tabs open has three live endpoints, all of
-- which must receive a push, not just the most recently registered one.
--
-- Uniqueness is scoped to (environment, subscriber, endpoint): re-registering
-- the same browser subscription (a service worker refresh, e.g.) upserts the
-- existing row in place rather than accumulating duplicates.

CREATE TABLE webpush_subscriptions (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    subscriber_id  TEXT NOT NULL,
    endpoint       TEXT NOT NULL,
    p256dh         TEXT NOT NULL,
    auth           TEXT NOT NULL,
    created_at     TEXT NOT NULL DEFAULT '',
    last_used_at   TEXT NOT NULL DEFAULT '',
    UNIQUE (environment_id, subscriber_id, endpoint)
);

CREATE INDEX idx_webpush_subscriptions_subscriber ON webpush_subscriptions (org_id, environment_id, subscriber_id);
