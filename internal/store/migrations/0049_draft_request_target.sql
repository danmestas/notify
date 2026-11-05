-- 0049_draft_request_target: a review-mode draft request records the agent and
-- the conversation (its platform thread id) it was made for, so Produce can
-- tell the turn the request is for from any other activity that merely
-- carries the same client_message_id (danmestas/notify#87).
--
-- Both columns are nullable. A request written before this migration has
-- neither and is matched on its key alone, as before; it stays pending until
-- retention purges it. A request with the columns set is produced only by a
-- turn of that agent in that conversation.
ALTER TABLE agent_draft_requests ADD COLUMN agent_identifier TEXT;
ALTER TABLE agent_draft_requests ADD COLUMN conversation_id TEXT;
