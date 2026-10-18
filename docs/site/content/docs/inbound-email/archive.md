---
title: Archive
weight: 2
---

notify can keep the **original bytes** of mail, exactly as received, in a blob store. This is off unless you turn it on, because keeping every accepted email is a retention decision.

## Turning it on

Set `NOTIFY_BLOB_DRIVER`:

| Value | Store |
|---|---|
| empty (default) | Off. Nothing is kept, and the archive routes are not mounted. |
| `fs` | Files in a local directory: `NOTIFY_BLOB_DIR`, default a `blobs` directory next to the SQLite file (`<dir of NOTIFY_DB>/blobs`). |
| `s3` | An S3-API bucket: `NOTIFY_BLOB_S3_*`. |

Any other value stops notify at start. Setup, including a self-hosted Garage, is under [Blob storage](../../operations/blob-storage).

Objects are addressed by the SHA-256 of their bytes. Storing the same bytes twice is a no-op, and a reader can check what it got against the address.

## What gets kept

- **Mail accepted by the reply loop.** The raw message is stored *before* the message is routed. If the store fails, the transport is told to retry (HTTP `500`, SMTP `451`), so mail is never accepted without its bytes, and the retry stores nothing twice. The SMTP receiver always has the raw bytes; the JSON webhook keeps them only if the forwarder sends `raw`.
- **Mail a client already received**, sent to the archive endpoint below.

Each kept message gets a row with its id, hash, size, recipient, sender, subject, `Message-ID` and time.

## The archive endpoints

All take an API key and are mounted only when a blob store is configured.

### Archive a message

`POST /v1/inbound/messages?recipient=<address>` with the message itself as the body (RFC 5322 bytes, `message/rfc822`).

```bash
curl -X POST "localhost:3000/v1/inbound/messages?recipient=ada@acme.test" \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: message/rfc822' \
  --data-binary @message.eml
```

It is storage only: the message is not routed anywhere. It is **idempotent**: the same bytes for the same recipient return the record already kept. The response is `200` with the record:

```json
{ "id": "01J…", "sha256": "…", "size": 4821, "recipient": "ada@acme.test",
  "from": "Ann <ann@example.com>", "subject": "Invoice", "messageId": "<…@example.com>",
  "receivedAt": "2026-10-18T12:00:00Z" }
```

| Status | Meaning |
|---|---|
| `400` | `recipient` is missing, or the body is empty. |
| `413` | The message is over **40 MiB**. (Mail services accept up to about 25 MiB, which base64 attachments already account for; the rest is headroom.) |

A message whose headers do not parse is still kept, with `from`, `subject` and `messageId` empty. The archive keeps what arrived.

### List

`GET /v1/inbound/messages?after=<cursor>&limit=<n>` lists messages **oldest first**. `limit` defaults to 50. `after` is the opaque cursor from the previous response; a bad cursor is `400`.

```json
{ "messages": [ { "id": "…", "sha256": "…", "size": 4821, "recipient": "…", "from": "…", "subject": "…", "messageId": "…", "receivedAt": "…" } ],
  "cursor": "17" }
```

An empty page returns the cursor you sent, so you can poll by passing it back.

### Fetch the original

`GET /v1/inbound/messages/{id}/raw` streams the bytes as `message/rfc822`, with `Content-Length` and `X-Content-SHA256` headers. `404` if the id is unknown, or if the bytes are no longer in the store.
