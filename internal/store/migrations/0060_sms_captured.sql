-- 0060_sms_captured: texts a capture SMS integration accepted instead of
-- sending (#173). Kept so a tenant can read back what a workflow sent; the
-- newest rows per integration are kept, and an integration's rows go with it.
CREATE TABLE sms_captured (
  seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
  id                  TEXT NOT NULL UNIQUE,
  org_id              TEXT NOT NULL,
  environment_id      TEXT NOT NULL,
  integration_id      TEXT NOT NULL,
  provider_message_id TEXT NOT NULL UNIQUE,
  to_phone            TEXT NOT NULL,
  from_phone          TEXT NOT NULL,
  body                TEXT NOT NULL,
  created_at          TEXT NOT NULL
);
CREATE INDEX sms_captured_integration ON sms_captured (integration_id, seq);
