-- 0058_sms_operator_consent: operator-attested SMS consent (#142).

-- Whether the environment lets an API key attest that a subscriber consented
-- to texts, and the privacy policy URL that consent rests on. Off by default;
-- turning it on requires the URL. Turning it off suspends every attestation
-- in the environment without clearing it.
ALTER TABLE environments ADD COLUMN sms_consent_attestation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE environments ADD COLUMN sms_consent_policy_url TEXT NOT NULL DEFAULT '';

-- Whether this key may attest SMS consent. Every key written before this
-- migration may not.
ALTER TABLE api_keys ADD COLUMN attest_sms_consent INTEGER NOT NULL DEFAULT 0;

-- When an operator last attested consent for the subscriber's phone; empty
-- means no attestation. Cleared, like phone_verified_at, by any write that
-- changes the phone, by a revoke and by soft delete.
ALTER TABLE subscribers ADD COLUMN phone_attested_at TEXT NOT NULL DEFAULT '';

-- One row per attestation, kept for audit. revoked_at is set, never the row
-- deleted, when the attestation stops counting: revoked through the API,
-- replaced by a newer attestation, or cleared by a phone change or delete.
CREATE TABLE sms_consent_attestations (
  id             TEXT PRIMARY KEY,
  org_id         TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  subscriber_id  TEXT NOT NULL,
  phone          TEXT NOT NULL,
  api_key_id     TEXT NOT NULL,
  source         TEXT NOT NULL DEFAULT '',
  consented_at   TEXT NOT NULL DEFAULT '',
  policy_url     TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  revoked_at     TEXT NOT NULL DEFAULT '',
  revoked_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sms_consent_attestations_subscriber
  ON sms_consent_attestations (org_id, environment_id, subscriber_id, revoked_at);
