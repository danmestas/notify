-- 0019_chat_threads: the chat two-way loop's correlation table. One row per
-- chat-origin conversation, mapping the conversation's platform_thread_id to
-- the provider (telegram | discord) and the chat_id the user messaged from
-- (Telegram chat_id / Discord channel_id). Inbound chat dials out (Telegram
-- getUpdates long-poll, Discord gateway WS), routes a message by its chat_id to
-- the bound agent + conversation, and threads the reply onto it; the agent
-- reply path reads this row to send the answer back into the same chat,
-- closing the loop.
--
-- Two lookups: inbound routing keys on (org, env, provider, chat_id) — a chat
-- has at most one binding within a tenant; the ReplyHook reverse lookup keys on
-- platform_thread_id (the conversation's thread id).

CREATE TABLE chat_threads (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  agent_identifier TEXT NOT NULL,          -- the responder agent for this thread
  platform_thread_id TEXT NOT NULL,        -- the conversation's inbox thread id
  subscriber_id TEXT NOT NULL,             -- the human on the chat side (tg:<id> / dc:<id>)
  provider TEXT NOT NULL,                  -- telegram | discord
  chat_id TEXT NOT NULL,                   -- Telegram chat_id / Discord channel_id
  created_at TEXT NOT NULL,
  last_activity_at TEXT NOT NULL,
  UNIQUE(org_id, environment_id, provider, chat_id));

CREATE INDEX chat_threads_thread ON chat_threads(platform_thread_id);
