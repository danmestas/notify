-- 0007_topics: named subscriber sets per (environment). A topic is uniquely
-- identified by (environment_id, key); triggering a workflow "to" a topic
-- fans out to every subscriber recorded in topic_subscribers (multicast).

CREATE TABLE topics (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    environment_id  TEXT NOT NULL,
    key             TEXT NOT NULL,
    name            TEXT,
    UNIQUE (environment_id, key)
);

CREATE TABLE topic_subscribers (
    topic_id        TEXT NOT NULL,
    subscriber_id   TEXT NOT NULL,
    environment_id  TEXT NOT NULL,
    PRIMARY KEY (topic_id, subscriber_id)
);

CREATE INDEX topic_subscribers_env_topic ON topic_subscribers (environment_id, topic_id);
