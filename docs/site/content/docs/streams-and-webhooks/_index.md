---
title: Streams and webhooks
weight: 8
---

Three ways to move events in and out of notify besides messages to people: a durable **feed** you can subscribe to, **stream sources** that push events into it, and **outbound webhooks** that carry delivery events to your servers.

## The feed

The feed is a durable JetStream stream per environment. Two things write to it: the [`stream` step](../workflows/step-types/stream), and stream sources. Read it as Server-Sent Events:

```bash
curl -N "localhost:3000/v1/feed/stream?topic=orders.eu&from=2026-10-18T00:00:00Z" \
  -H "Authorization: ApiKey $KEY"
```

| Query | Meaning |
|---|---|
| `topic` | Required. A concrete topic (no wildcards): dot-separated tokens of letters, digits, `_`, `-`; at most 128 bytes. `400` otherwise. |
| `from` | Where to start: a stream sequence number or an RFC 3339 time. Absent means live only. |

To resume after a disconnect, send `Last-Event-ID` with the last event id you saw; it takes precedence over `from`. Unlike the inbox stream, the feed is **lossless**: a client that falls too far behind is disconnected rather than silently dropped, and it resumes where it left off. The environment comes only from your API key.

Two payload shapes share the feed, told apart by the `v` key. A `stream` step publishes the message JSON (`{"Channel": "stream", …}`); a source publishes the envelope below.

## Stream sources

A stream source lets an external system push events into the feed without an API key. It holds a `SourceKey` secret that works for one endpoint only.

Sources are created and managed in the console (Stream sources): create, rotate the key, set limits, deactivate, delete. The secret is shown when you create or rotate it. Defaults and ceilings:

| Limit | Default | Maximum |
|---|---|---|
| Events per minute | 600 | 6000 |
| Bytes per hour | 2 MiB | 16 MiB |
| Events per batch | 100 | |
| Bytes per event | 32 KiB | |

A source has a base `topic`; a producer may publish to that topic or any dot-extension of it.

### Pushing events

```bash
curl -X POST localhost:3000/v1/sources/$SOURCE_ID/events \
  -H "Authorization: SourceKey $SOURCE_KEY" -H 'Content-Type: application/json' \
  -d '{"topic": "builds.api", "events": [{"id": "run-881", "ts": "2026-10-18T12:00:00Z", "data": {"status": "passed"}}]}'
```

`topic` is optional (defaults to the source topic). Each event has `data` (required, any JSON) and optional `id` (up to 128 characters of `A-Za-z0-9_-`; makes the event deduplicated, so a retry is safe) and `ts` (RFC 3339; stamped now if absent). Consumers receive:

```json
{ "v": 1, "source_id": "01J…", "event_id": "run-881", "ts": "2026-10-18T12:00:00Z", "data": {"status": "passed"} }
```

| Status | Meaning |
|---|---|
| `202` | Accepted: `{"accepted": n}`. |
| `400` | Bad JSON, empty `events`, invalid `data`, `id` or `ts`, or a topic that is not the source's or an extension of it. |
| `401` | Missing or wrong key. (All authentication failures look the same.) |
| `403` | The source is inactive. |
| `404` | A valid key, but for a different source id. |
| `413` | Body over 1 MiB, more events than the batch limit, or an event over its size limit. |
| `429` | Rate limit or byte budget exceeded; `Retry-After` is `60` or `3600`. |
| `502` | The broker failed mid-batch; `accepted` says how many were stored. Retry the whole batch (ids make that safe). |
| `503` | The limiter is unavailable. |

Every rejection happens before anything is published.

## Outbound webhooks

Outbound webhook endpoints receive **delivery events**: the message JSON of every terminal channel result (`sent`, `error`, `skipped`, `throttled`), and `email.reply` events from the [reply loop](../inbound-email/reply-loop).

```bash
curl -X POST localhost:3000/v1/webhooks/endpoints \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"url": "https://hooks.acme.test/notify", "active": true}'
```

The response includes the endpoint (`id`, `url`, `active`, `signed`) and, **once**, its signing secret. `POST /v1/webhooks/endpoints/{id}/secret` rotates it and returns the new one; endpoints that predate signing get their first secret this way. Management routes are in the [API reference](../api/streams-and-webhooks).

### Delivery

Each event is POSTed as JSON to every active endpoint in the event's environment.

- **Signature.** Headers `X-Notify-Timestamp` and `X-Notify-Signature: sha256=<hex>`, where the signature is `HMAC-SHA256(secret, timestamp + "." + body)`. Every attempt is signed afresh. Reject events whose timestamp is too old to bound replay.
- **At least once.** Events are consumed from a durable JetStream consumer, so events published while notify is down are delivered when it comes back.
- **Per-endpoint retry.** A failed POST is retried with exponential backoff (5 seconds, doubling, capped at 30 minutes) for up to 12 attempts, about an hour and three quarters, then dropped with an error log. Retries are per endpoint and survive restarts; an endpoint that is down does not delay the others.
- **Disabled or deleted endpoints** drop their pending deliveries.
- **SSRF guard.** URLs are checked at connect time like other tenant-supplied URLs ([details](../workflows/step-types/http-request#network-restrictions)).

An `email.reply` event:

```json
{ "type": "email.reply", "subscriberId": "user-1", "transactionId": "…", "workflow": "order-shipped",
  "notificationId": "…", "text": "Thanks!", "from": "ada@example.com", "messageId": "<…>", "receivedAt": "…" }
```
