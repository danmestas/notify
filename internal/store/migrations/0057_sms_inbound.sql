-- 0057_sms_inbound: inbound SMS, delivery-status callbacks and opt-out
-- keywords (#143).

-- The id the provider gave a message at send (Twilio/SignalWire MessageSid). A
-- status callback names the message by it, within its integration. Empty for
-- every row written before this migration and for channels that do not report
-- one back.
ALTER TABLE messages ADD COLUMN provider_message_id TEXT NOT NULL DEFAULT '';
CREATE INDEX messages_provider_message ON messages (environment_id, integration_id, provider_message_id)
  WHERE provider_message_id <> '';

-- Inbound texts share the inbound message store with inbound mail. A text has
-- no blob: its body is kept inline, and sha256/size describe that body. A
-- text is kept once per provider message id within its integration, so a
-- provider retry adds nothing; mail keeps deduplicating by content and
-- recipient.
ALTER TABLE inbound_messages ADD COLUMN channel TEXT NOT NULL DEFAULT 'email';
ALTER TABLE inbound_messages ADD COLUMN body TEXT NOT NULL DEFAULT '';
ALTER TABLE inbound_messages ADD COLUMN integration_id TEXT NOT NULL DEFAULT '';
ALTER TABLE inbound_messages ADD COLUMN subscriber_id TEXT NOT NULL DEFAULT '';
ALTER TABLE inbound_messages ADD COLUMN phone_verified INTEGER NOT NULL DEFAULT 0;
DROP INDEX inbound_messages_once;
CREATE UNIQUE INDEX inbound_messages_once ON inbound_messages (org_id, environment_id, sha256, recipient)
  WHERE channel = 'email';
CREATE UNIQUE INDEX inbound_messages_sms_once ON inbound_messages (org_id, environment_id, integration_id, message_id)
  WHERE channel = 'sms';

-- A number that texted STOP (or a synonym) to an integration in the
-- environment. No sms step sends to it until it texts START. Keyed by the
-- number, not a subscriber, so it holds for every subscriber with that number
-- and for one added later.
CREATE TABLE sms_opt_outs (
  org_id         TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  phone          TEXT NOT NULL,
  integration_id TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  PRIMARY KEY (org_id, environment_id, phone)
);

-- Inbound texts find their sender's subscribers by phone.
CREATE INDEX subscribers_phone ON subscribers (environment_id, phone) WHERE phone <> '';

-- A final delivery status (delivered or error) that arrived before the
-- message it names was written: the provider can call back before the send's
-- message row commits. It is applied, and removed, when a message with that
-- provider id is written in the same environment and integration. Rows older
-- than an hour are dropped unapplied.
CREATE TABLE sms_status_pending (
  org_id              TEXT NOT NULL,
  environment_id      TEXT NOT NULL,
  integration_id      TEXT NOT NULL,
  provider_message_id TEXT NOT NULL,
  status              TEXT NOT NULL,
  error_class         TEXT NOT NULL DEFAULT '',
  error_text          TEXT NOT NULL DEFAULT '',
  received_at         TEXT NOT NULL,
  PRIMARY KEY (org_id, environment_id, integration_id, provider_message_id)
);
CREATE INDEX sms_status_pending_age ON sms_status_pending (received_at);

-- The last HELP reply sent to a number through an integration, so a number
-- texting HELP repeatedly gets one reply per window.
CREATE TABLE sms_help_replies (
  org_id         TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  integration_id TEXT NOT NULL,
  phone          TEXT NOT NULL,
  sent_at        TEXT NOT NULL,
  PRIMARY KEY (org_id, environment_id, integration_id, phone)
);
