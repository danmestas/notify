-- 0052_analytics_event_key: idempotent analytics ingest. The projector now
-- consumes a durable JetStream consumer, which delivers at least once, so each
-- row carries the delivery event's identity (message id + status, see
-- eventbus.DeliveryEventKey) and a redelivery is dropped by the unique index.
-- Rows written before this migration keep a NULL key; NULLs never collide.
ALTER TABLE analytics_deliveries ADD COLUMN event_key TEXT;

CREATE UNIQUE INDEX analytics_deliveries_event_key ON analytics_deliveries (event_key);
