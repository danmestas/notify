-- 0054_sms_verified_phone: a subscriber's phone is verified only by the
-- subscriber (#130). phone_verified_at is set by a confirmed code and cleared
-- by any write that changes the number; empty means unverified, which is every
-- row written before this migration. SMS steps send only to a verified phone.
ALTER TABLE subscribers ADD COLUMN phone_verified_at TEXT NOT NULL DEFAULT '';

-- One row per verification code sent. Only a salted hash of the code is kept.
-- The newest row for a subscriber is the pending one; older rows stay until
-- pruned so sends and wrong guesses can be limited per subscriber, per number
-- and per environment. consumed means the code can no longer be confirmed
-- (used, or its subscriber was deleted); verified means it was confirmed, so
-- attempts - verified is the wrong guesses made against it.
CREATE TABLE phone_verifications (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    subscriber_id  TEXT NOT NULL,
    phone          TEXT NOT NULL,
    code_hash      TEXT NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 0,
    consumed       INTEGER NOT NULL DEFAULT 0,
    verified       INTEGER NOT NULL DEFAULT 0,
    expires_at     TEXT NOT NULL,
    created_at     TEXT NOT NULL
);

CREATE INDEX phone_verifications_subscriber ON phone_verifications (environment_id, subscriber_id, created_at);
CREATE INDEX phone_verifications_phone ON phone_verifications (environment_id, phone, created_at);
CREATE INDEX phone_verifications_env ON phone_verifications (environment_id, created_at);
