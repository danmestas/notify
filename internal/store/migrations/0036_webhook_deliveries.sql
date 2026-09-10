-- 0036_webhook_deliveries: the outbound webhook outbox (#346). One row per
-- (endpoint, event) is written when an event is consumed off the delivery
-- stream, and deleted when the endpoint accepts it or the retries run out.
-- Each row keeps its own clock, so a dead endpoint backs off on its own
-- while every other endpoint keeps flowing.
CREATE TABLE webhook_deliveries (
  id              TEXT PRIMARY KEY,
  org_id          TEXT NOT NULL,
  environment_id  TEXT NOT NULL,
  endpoint_id     TEXT NOT NULL,
  payload         BLOB NOT NULL,
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  created_at      TEXT NOT NULL
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at);
