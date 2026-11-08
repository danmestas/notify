-- 0053_integration_health_check_detail: keep why the last credential check
-- failed. A check that fails for a reason other than a rejected credential (an
-- unreachable server, a timeout) leaves the state as it is, so without this
-- its reason was lost. Redacted and truncated before it is stored.
ALTER TABLE integration_health ADD COLUMN last_check_detail TEXT NOT NULL DEFAULT '';
