-- 0035_webhook_endpoint_secret: a per-endpoint signing secret so a receiver can
-- tell a notify event from anything else that POSTs JSON at it (#346). Stored
-- SEALED (secretbox under the master key), never in the clear. Empty is an
-- endpoint that predates signing, which keeps receiving unsigned events until
-- its secret is rotated in.
ALTER TABLE webhook_endpoints ADD COLUMN secret TEXT NOT NULL DEFAULT '';
