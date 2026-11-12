-- 0059_sms_consent_retention: retention of SMS consent audit rows (#164).

-- The retention purge deletes rows by revoked_at; open rows hold the empty string.
CREATE INDEX sms_consent_attestations_revoked
  ON sms_consent_attestations (revoked_at);
