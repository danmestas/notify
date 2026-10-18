-- 0041_agent_labels: free-form key/value labels on an agent, e.g.
-- {"owner":"lou"}, so a client can scope which agents a mailbox or user may
-- pick. notify only stores and filters them; it gives no label a meaning.
ALTER TABLE agents ADD COLUMN labels TEXT NOT NULL DEFAULT '{}';
