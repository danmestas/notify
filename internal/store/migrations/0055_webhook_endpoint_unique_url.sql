-- 0055_webhook_endpoint_unique_url: an environment has at most one ACTIVE
-- endpoint per URL (#141), so setup by URL is idempotent.
--
-- Rows written before this migration may hold duplicates, and each may carry a
-- signing secret a live integration depends on, so nothing is deleted. Within
-- each (org, environment, URL) group (URL compared after trimming) the most
-- recently created active row is kept as the live one (ids are ULIDs, so the
-- highest id is the newest); if none is active, the newest row is the one the
-- upsert matches. Every other row in the group is set active = 0, with its
-- row and secret left intact. Deactivated duplicates still appear in
-- GET /v1/webhooks/endpoints (active = false) and can be inspected there or
-- deleted by hand; to find them in SQL, select rows sharing
-- (org_id, environment_id, url) with active = 0.
UPDATE webhook_endpoints SET url = trim(url);

UPDATE webhook_endpoints SET active = 0 WHERE id IN (
    SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (
            PARTITION BY org_id, environment_id, url ORDER BY active DESC, id DESC
        ) AS rn
        FROM webhook_endpoints
    ) WHERE rn > 1
);

CREATE UNIQUE INDEX webhook_endpoint_env_url ON webhook_endpoints (org_id, environment_id, url) WHERE active = 1;
