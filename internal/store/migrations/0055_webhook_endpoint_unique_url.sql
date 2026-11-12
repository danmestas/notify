-- 0055_webhook_endpoint_unique_url: an environment has at most one endpoint per
-- URL (#141), so setup by URL is idempotent. Rows written before this
-- migration may hold duplicates. Within each (org, environment, url) group the
-- oldest active row is kept (ids are ULIDs, so lowest id is oldest; an active
-- row beats an inactive one, and if none is active the oldest inactive stays)
-- and the rest are deleted. Their signing secrets are discarded with them.
UPDATE webhook_endpoints SET url = trim(url);

DELETE FROM webhook_endpoints WHERE id IN (
    SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (
            PARTITION BY org_id, environment_id, url ORDER BY active DESC, id ASC
        ) AS rn
        FROM webhook_endpoints
    ) WHERE rn > 1
);

CREATE UNIQUE INDEX webhook_endpoint_env_url ON webhook_endpoints (org_id, environment_id, url);
