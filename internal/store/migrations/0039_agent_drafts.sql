-- 0039_agent_drafts: an agent's reply to an email, held as a draft for the
-- mailbox owner to review and send. notify never sends it. seq is the poll
-- cursor: it increases in commit order, so a reader paging by it never skips
-- a draft. A draft is keyed to the agent activity it came from, so producing it
-- twice yields one row.
CREATE TABLE agent_drafts (
  seq               INTEGER PRIMARY KEY AUTOINCREMENT,
  id                TEXT NOT NULL UNIQUE,
  org_id            TEXT NOT NULL,
  environment_id    TEXT NOT NULL,
  subscriber_id     TEXT NOT NULL,
  mailbox           TEXT NOT NULL,
  agent_identifier  TEXT NOT NULL,
  agent_name        TEXT NOT NULL DEFAULT '',
  conversation_id   TEXT NOT NULL,
  activity_id       TEXT NOT NULL,
  source_message_id TEXT NOT NULL DEFAULT '',
  in_reply_to       TEXT NOT NULL DEFAULT '',
  refs              TEXT NOT NULL DEFAULT '[]',
  subject           TEXT NOT NULL DEFAULT '',
  recipients        TEXT NOT NULL DEFAULT '[]',
  text              TEXT NOT NULL,
  created_at        TEXT NOT NULL
);
CREATE UNIQUE INDEX agent_drafts_activity ON agent_drafts(org_id, environment_id, activity_id);
CREATE INDEX agent_drafts_poll ON agent_drafts(org_id, environment_id, subscriber_id, mailbox, seq);
