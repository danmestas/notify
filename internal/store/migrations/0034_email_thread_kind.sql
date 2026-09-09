-- 0034_email_thread_kind: a reply thread can now be answered by something other
-- than an agent. kind selects the responder ("agent" is every row that existed
-- before this migration; "webhook" hands the reply to the environment's
-- outbound webhook endpoints as an email.reply event). transaction_id and
-- workflow_identifier are carried on webhook-kind rows so the event can name
-- what the recipient replied to without a second lookup; the thread itself is
-- keyed on the per-recipient notification id, because a multicast trigger
-- shares one transaction id across every recipient.
ALTER TABLE email_threads ADD COLUMN kind TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE email_threads ADD COLUMN transaction_id TEXT NOT NULL DEFAULT '';
ALTER TABLE email_threads ADD COLUMN workflow_identifier TEXT NOT NULL DEFAULT '';
