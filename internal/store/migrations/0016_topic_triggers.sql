-- 0016_topic_triggers: projection table for topic-keyed multicast triggers.
-- Records (transaction_id, topic_key) at trigger time so the Topic Activity
-- Feed can JOIN analytics_deliveries ON transaction_id without modifying the
-- hot analytics table (Option A from spec-04 BP-AF-TOPIC).
CREATE TABLE IF NOT EXISTS topic_triggers (
    id             TEXT NOT NULL PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    topic_key      TEXT NOT NULL,
    transaction_id TEXT NOT NULL,
    created_at     TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_topic_triggers_txn
    ON topic_triggers (environment_id, transaction_id);

CREATE INDEX IF NOT EXISTS idx_topic_triggers_key
    ON topic_triggers (environment_id, topic_key, created_at DESC);
