---
title: Streams and webhooks
weight: 7
---

Concepts: [Streams and webhooks](../../streams-and-webhooks).

| Method and path | Auth | Purpose |
|---|---|---|
| `GET /v1/feed/stream?topic=&from=` | API key | SSE over the durable feed. Resumes from `Last-Event-ID`. |
| `POST /v1/sources/{sourceID}/events` | `SourceKey` | Push a batch of events. `202 {"accepted": n}`. |
| `GET /v1/webhooks/endpoints` | API key | List outbound endpoints: `{data: […]}`. |
| `POST /v1/webhooks/endpoints` | API key | Set up `{"url", "active", "secret"?}`. Idempotent by URL: `201` with the endpoint and its secret, shown once, or `200` with the existing endpoint. |
| `GET /v1/webhooks/endpoints/{id}` | API key | One endpoint. |
| `PUT /v1/webhooks/endpoints/{id}` | API key | Update `{"url", "active"?}`; `url` required. |
| `DELETE /v1/webhooks/endpoints/{id}` | API key | Delete. `204`. |
| `POST /v1/webhooks/endpoints/{id}/secret` | API key | Rotate the signing secret, to `{"secret"?}` or a generated one; the new one is returned once. |

An endpoint: `{id, url, active, signed}`; the create and rotate responses add `secret`. Events are signed with `X-Notify-Timestamp` and `X-Notify-Signature: sha256=<hex>`. Stream sources have no `/v1` management routes: create them in the console.


## Idempotent setup

`POST /v1/webhooks/endpoints` is safe to run on every deploy. An environment has at most one active endpoint per URL, matched after trimming the URL and lowercasing its scheme and host (the path and query are case-sensitive). When the URL already exists the call returns `200` with that endpoint, creates nothing, and does not rotate or reveal its secret. `active` is ignored for an existing endpoint; change it with `PUT`. The same URL in two environments is two endpoints. A `PUT` that would make an endpoint active under a URL another active endpoint holds is a `409`.

## Supplying the signing secret

`secret` is optional on create and on rotate. Without it, notify generates 32 random bytes as hex. A supplied secret must be 32 to 256 characters with at least 8 distinct characters, or the call is a `400`. It is stored sealed, like a generated one, and is never returned by a read; the response to the call that set it is the only one that carries `secret`.

To pin a secret you already manage, send it on setup. If the URL already exists, a supplied `secret` replaces that endpoint's secret and is echoed back in the `200`; omit `secret` to leave the existing one alone. Verify deliveries with `sha256=` plus the hex HMAC-SHA256, keyed by the secret, over `X-Notify-Timestamp` + `.` + the raw body.

Migration 0055 enforces the one-URL rule with a unique index on active endpoints. Nothing is deleted. Duplicates that predate it were resolved per environment and URL (compared after trimming): the most recently created active endpoint stays live, or the newest if none was active, and every other duplicate was set to `active: false` with its row and secret intact. List them with `GET /v1/webhooks/endpoints` (look for the same `url` with `active: false`) and remove any you no longer need with `DELETE`.
