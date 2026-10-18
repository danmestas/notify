-- 0042_inbound_messages: each inbound email notify accepted, with its original
-- bytes kept in the blob store under sha256. One row per message per
-- recipient, so a sender's retry of the same message adds nothing. seq is the
-- read cursor and rises in commit order.
CREATE TABLE inbound_messages (
  seq            INTEGER PRIMARY KEY AUTOINCREMENT,
  id             TEXT NOT NULL UNIQUE,
  org_id         TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  sha256         TEXT NOT NULL,
  size           INTEGER NOT NULL,
  recipient      TEXT NOT NULL,
  sender         TEXT NOT NULL DEFAULT '',
  subject        TEXT NOT NULL DEFAULT '',
  message_id     TEXT NOT NULL DEFAULT '',
  received_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX inbound_messages_once ON inbound_messages(org_id, environment_id, sha256, recipient);
CREATE INDEX inbound_messages_list ON inbound_messages(org_id, environment_id, seq);
