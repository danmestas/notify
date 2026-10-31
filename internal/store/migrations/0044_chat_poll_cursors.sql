-- 0044_chat_poll_cursors: where each chat poller (bluesky, mastodon,
-- telegram) left off, per tenant and provider. Without it a restart empties
-- the in-memory cursor and the poller replays recent mentions, and each
-- replay runs a fresh agent turn that posts a duplicate public reply.
-- cursor is opaque text: a Bluesky indexedAt, a Mastodon notification id, or
-- a Telegram update offset in decimal.
CREATE TABLE chat_poll_cursors (
  org_id         TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  provider       TEXT NOT NULL,
  cursor         TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  PRIMARY KEY (org_id, environment_id, provider));

-- chat_inbound_handled: one row per inbound chat message a poller has
-- claimed for dispatch, keyed by the platform's message id (Bluesky
-- notification uri, Mastodon notification id, Telegram update_id). The claim
-- is an insert that ignores conflicts, made before the agent turn runs, so a
-- reply goes out at most once per message even when the process dies between
-- dispatch and the cursor save. Rows are pruned by age.
CREATE TABLE chat_inbound_handled (
  org_id         TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  provider       TEXT NOT NULL,
  message_id     TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  PRIMARY KEY (org_id, environment_id, provider, message_id));
CREATE INDEX chat_inbound_handled_age ON chat_inbound_handled(created_at);
