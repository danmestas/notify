-- 0018_email_threads: the email two-way loop's correlation table. One row per
-- email-origin conversation, mapping the conversation's platform_thread_id to
-- the recipient's email address and the verified inbound domain that owns the
-- tokenized reply-to mailbox. The inbound-parse endpoint resolves a reply token
-- back to (org, env, agent, thread, subscriber) and threads the reply onto the
-- conversation; the agent reply path reads this row to mail the answer back out
-- with the same tokenized Reply-To, closing the loop.
--
-- Keyed by (org_id, environment_id, platform_thread_id): a conversation thread
-- has at most one email binding within a tenant.

CREATE TABLE email_threads (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  agent_identifier TEXT NOT NULL,          -- the responder agent for this thread
  platform_thread_id TEXT NOT NULL,        -- the conversation's inbox thread id
  subscriber_id TEXT NOT NULL,             -- the human on the email side
  subscriber_email TEXT NOT NULL,          -- where outbound replies are mailed
  inbound_domain TEXT NOT NULL,            -- verified domain owning the reply-to mailbox
  created_at TEXT NOT NULL,
  last_activity_at TEXT NOT NULL,
  UNIQUE(org_id, environment_id, platform_thread_id));

CREATE INDEX email_threads_thread ON email_threads(platform_thread_id);
