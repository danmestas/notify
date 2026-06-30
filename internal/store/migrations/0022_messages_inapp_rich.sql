-- Phase-A Wave 3 (in-app-rich): add an `extras` JSON snapshot column to messages
-- so an in_app message can carry rich fields (avatar, primaryAction,
-- secondaryAction, redirect, data) captured at send time and returned verbatim
-- by the inbox API + SSE. The blob is stored, never queried, so no index.
--
-- NOT NULL DEFAULT '{}' is safe for SQLite ALTER TABLE ADD COLUMN: existing rows
-- backfill to '{}', which the domain layer unmarshals to an empty object (no rich
-- fields). The repo coerces a nil Extras to '{}' before INSERT so the skipped/
-- throttled/errored delivery paths — which never set Extras — keep persisting.

ALTER TABLE messages ADD COLUMN extras TEXT NOT NULL DEFAULT '{}';
