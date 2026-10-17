-- 0040_agent_draft_requests: an agent_message step in review mode records,
-- before it composes, that the agent's reply to this message must become a
-- draft, with the mail context the draft carries. It is keyed by the composed
-- message's idempotency key, so the turn finds it from the message it answers.
-- draft_id is set once the draft exists, so a retried turn produces no second
-- draft.
CREATE TABLE agent_draft_requests (
  org_id            TEXT NOT NULL,
  environment_id    TEXT NOT NULL,
  client_message_id TEXT NOT NULL,
  subscriber_id     TEXT NOT NULL,
  mailbox           TEXT NOT NULL,
  source_message_id TEXT NOT NULL,
  in_reply_to       TEXT NOT NULL DEFAULT '',
  refs              TEXT NOT NULL DEFAULT '[]',
  subject           TEXT NOT NULL DEFAULT '',
  recipients        TEXT NOT NULL DEFAULT '[]',
  draft_id          TEXT,
  created_at        TEXT NOT NULL,
  PRIMARY KEY (org_id, environment_id, client_message_id)
);
