-- 0010_agents: agent communication (RFC 0002 §2, verbatim). Agents are
-- conversational endpoints keyed by (environment_id, identifier); agent_links
-- bind them to channel integrations; conversations + conversation_activities
-- are the durable transcript (execution state stays in the engine).

CREATE TABLE agents (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL, environment_id TEXT NOT NULL,
  identifier TEXT NOT NULL, name TEXT, description TEXT,
  runtime TEXT NOT NULL,                       -- mesh|bridge|managed
  active INT DEFAULT 1,
  mesh_subject TEXT,                           -- runtime=mesh: prompt subject
  bridge_url TEXT,                             -- runtime=bridge
  managed JSON,                                -- runtime=managed: {integration_id,
                                               --  system_prompt, tools[], mcp_servers[], skills[]}
  welcome_message TEXT, created_at TEXT, updated_at TEXT,
  UNIQUE(environment_id, identifier));

CREATE TABLE agent_links (                     -- agent <-> channel integration
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL, environment_id TEXT NOT NULL,
  agent_id TEXT NOT NULL, integration_id TEXT NOT NULL,
  connected_at TEXT, config JSON,
  UNIQUE(agent_id, integration_id));

CREATE TABLE conversations (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL, environment_id TEXT NOT NULL,
  agent_id TEXT NOT NULL, integration_id TEXT,
  platform_thread_id TEXT NOT NULL,            -- slack thread_ts | tg chat id | inbox conv id
  subscriber_id TEXT,                          -- the human, when resolvable
  status TEXT DEFAULT 'open',
  created_at TEXT, last_activity_at TEXT,
  UNIQUE(agent_id, integration_id, platform_thread_id));

CREATE TABLE conversation_activities (         -- append-only turn log
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL, environment_id TEXT NOT NULL,
  conversation_id TEXT NOT NULL,
  role TEXT NOT NULL,                          -- user|agent|system|action
  content JSON NOT NULL,                       -- {parts:[{text}], action?, attachments?}
  run_id TEXT,                                 -- dagnats turn run, when role=agent
  created_at TEXT);
CREATE INDEX conv_hist ON conversation_activities(conversation_id, created_at);
