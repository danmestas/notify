---
title: Backups and retention
weight: 4
---

## What to back up

| What | Where | Why |
|---|---|---|
| The SQLite database | `NOTIFY_DB` (default `notify.db`) | Everything notify owns: tenants, API key hashes, workflows and their versions, subscribers, integrations (encrypted), messages, agents and conversations, drafts. |
| **The master key** | Your secret store | Credentials, API keys, environment variables, session cookies and managed-agent keys are encrypted with it. **A backup of the database without the key is not restorable.** Store it separately from the database backup. |
| The blob store | `NOTIFY_BLOB_DIR` or the S3 bucket | Raw mail bytes, if you turned the archive on. The database holds only the records. |
| The dagnats data directory | `DAGNATS_DATA_DIR` | The JetStream streams (delivery events, the feed) and in-flight runs. Workflow definitions are re-registered from notify's database at start, so this is not needed to recover your configuration, but runs that were in flight are lost without it. |
| `DAGNATS_BRIDGE_TOKEN`, `NOTIFY_INBOUND_SECRET` | Your secret store | Needed to bring the pair back up. |

## Backing up SQLite

notify opens the database in WAL mode, so a plain `cp` of `notify.db` while it runs can miss recent writes that are still in the `-wal` file. Use SQLite's online backup, which is safe while notify is running:

```bash
sqlite3 /var/lib/notify/notify.db ".backup '/backups/notify-$(date +%F).db'"
```

or `VACUUM INTO`. Alternatively stop notify and copy the `.db`, `-wal` and `-shm` files together. Compose users back up the `notify-data` and `dagnats-data` volumes.

## Restoring

1. Stop notify.
2. Put the database back, and set the same `NOTIFY_MASTER_KEY_HEX`.
3. Start the dagnats sidecar, then notify. notify re-registers every workflow with the engine at start.

Run exactly one notify per database file.

## Retention

Several tables and streams are bounded so that nothing grows without limit. These are fixed in the source, not settings.

| Data | Kept | Purge |
|---|---|---|
| `http_request` captured responses (`step_outputs`) | 7 days | At start and every 6 hours. |
| Resolved dynamic delay deadlines (`delay_deadlines`) | 400 days (a year-long delay at 30 days per month, plus grace) | At start and daily. |
| Agent drafts | 30 days | At start and hourly. A reader whose cursor is older gets `gap: true`. |
| Delivery-status events (`NOTIFY_DELIVERY`) | 7 days | JetStream max age. |
| Feed events (`NOTIFY_FEED`) | 7 days and 1 GiB across all tenants | JetStream max age and size; oldest evicted first. A feed client can replay only within this window. |
| Outbound webhook retries | Up to 12 attempts over about an hour and three quarters | Then dropped with an error log. |

Messages, notifications, subscribers, conversations and archived mail are **not** purged. Delete them with the API or console, and manage the size of the database and blob store yourself.
