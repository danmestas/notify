-- 0009_layouts: reusable content wrappers keyed by (environment, key). A
-- channel step may reference a layoutKey; at render time the step's rendered
-- body is injected as {{content}} into the layout's content template, then the
-- layout is rendered.

CREATE TABLE layouts (
    id              TEXT PRIMARY KEY,
    org_id          TEXT,
    environment_id  TEXT NOT NULL,
    key             TEXT NOT NULL,
    name            TEXT,
    content         TEXT NOT NULL,
    UNIQUE (environment_id, key)
);
