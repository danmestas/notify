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
| `POST /v1/webhooks/endpoints` | API key | Create `{"url", "active"}`. `201` with the endpoint and its secret, shown once. |
| `GET /v1/webhooks/endpoints/{id}` | API key | One endpoint. |
| `PUT /v1/webhooks/endpoints/{id}` | API key | Update `{"url", "active"?}`; `url` required. |
| `DELETE /v1/webhooks/endpoints/{id}` | API key | Delete. `204`. |
| `POST /v1/webhooks/endpoints/{id}/secret` | API key | Rotate the signing secret; the new one is returned once. |

An endpoint: `{id, url, active, signed}`; the create and rotate responses add `secret`. Events are signed with `X-Notify-Timestamp` and `X-Notify-Signature: sha256=<hex>`. Stream sources have no `/v1` management routes: create them in the console.
