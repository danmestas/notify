-- 0045_agent_history: how a mesh turn carries the conversation. 'inline'
-- pastes the transcript into the prompt; 'none' sends only the new message
-- and relies on the agent's own state.
ALTER TABLE agents ADD COLUMN history TEXT NOT NULL DEFAULT 'inline';
