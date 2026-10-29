-- 0043_agent_logo: an optional logo on an agent, stored as a data: URI of a
-- png, jpeg or webp image (validated by the API, at most 96 KB decoded). Empty
-- means none.
ALTER TABLE agents ADD COLUMN logo TEXT NOT NULL DEFAULT '';
