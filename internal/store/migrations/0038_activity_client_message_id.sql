-- 0038_activity_client_message_id: a client-supplied id for a composed
-- message, so a retried POST of the same message is recognised instead of
-- appended again (danmestas/notify#3). Scoped to the conversation, and only
-- for rows that carry one: every existing row and every caller that sends no
-- id is unaffected.
ALTER TABLE conversation_activities ADD COLUMN client_message_id TEXT;
CREATE UNIQUE INDEX conv_client_msg ON conversation_activities(conversation_id, client_message_id)
  WHERE client_message_id IS NOT NULL;
