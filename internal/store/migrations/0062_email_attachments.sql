-- 0062_email_attachments: files a trigger hands to its email steps (#177).
-- The bytes live in the blob store under sha256; a row is the reference a
-- delivery reads them back by, so a retry resends the same file. One row per
-- recipient's notification, so a purge of a subscriber finds exactly that
-- subscriber's files; two rows with the same sha256 share one blob.
CREATE TABLE email_attachments (
  id              TEXT PRIMARY KEY,
  org_id          TEXT NOT NULL,
  environment_id  TEXT NOT NULL,
  notification_id TEXT NOT NULL,
  subscriber_id   TEXT NOT NULL,
  position        INTEGER NOT NULL,
  filename        TEXT NOT NULL,
  content_type    TEXT NOT NULL,
  sha256          TEXT NOT NULL,
  size            INTEGER NOT NULL,
  created_at      TEXT NOT NULL
);
-- A trigger retried after a crash writes the same rows again; position makes
-- that a no-op.
CREATE UNIQUE INDEX email_attachments_once ON email_attachments (notification_id, position);
CREATE INDEX email_attachments_subscriber ON email_attachments (org_id, environment_id, subscriber_id);
CREATE INDEX email_attachments_sha256 ON email_attachments (sha256);
