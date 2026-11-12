---
title: Inbound email
weight: 6
---

Concepts: [Inbound email](../../inbound-email).

| Method and path | Auth | Purpose |
|---|---|---|
| `POST /v1/inbound/email` | `X-Inbound-Secret` | Receive a parsed email from a forwarder. `200 {"status"}`; `401`; `503` when no secret is set. |
| `POST /v1/inbound/sms/{integrationId}` | Provider signature | An inbound text from Twilio or SignalWire ([SMS](../../channels/sms#inbound-texts-and-delivery-status)). `200` with empty TwiML; `403` bad or missing signature; `503` without `NOTIFY_PUBLIC_BASE_URL`. |
| `POST /v1/inbound/sms/{integrationId}/status` | Provider signature | A delivery-status callback. `204`; `403`; `503`. |
| `GET /v1/inbound/domains` | API key | List domains. `?search=&after=&limit=` (default limit 20). Returns `{data, next?, totalCount}`. |
| `POST /v1/inbound/domains` | API key | Register `{"name"}`; status `pending`. `201`; `409` if it exists. |
| `GET /v1/inbound/domains/{name}` | API key | One domain. `404` if absent. |
| `POST /v1/inbound/domains/{name}/verify` | API key | Live MX lookup; status becomes `verified` or `failed`. |
| `DELETE /v1/inbound/domains/{name}` | API key | Remove. `204`. |
| `POST /v1/inbound/messages?recipient=` | API key | [Archive](../../inbound-email/archive) a raw message (`message/rfc822` body, up to 40 MiB, idempotent). `200` the record. |
| `GET /v1/inbound/messages?after=&limit=` | API key | List archived mail and inbound texts, oldest first. `{messages, cursor}`. Each has `channel` (`email` or `sms`); a text also has `body`, `integrationId`, and `subscriberId`/`phoneVerified` when its sender matched a subscriber. |
| `GET /v1/inbound/messages/{id}/raw` | API key | The original bytes; for a text, its body as `text/plain`. |

The three `/v1/inbound/messages` routes exist only when `NOTIFY_BLOB_DRIVER` is set.

An inbound domain:

```json
{ "id": "…", "name": "reply.acme.test", "status": "pending", "mxConfigured": false,
  "expectedMXHost": "inbound.notify.example.com", "expectedMXPriority": 10,
  "createdAt": "…", "updatedAt": "…" }
```

`POST /v1/inbound/{trigger}` also lives under this prefix but is a workflow trigger; see [Workflows and triggers](../workflows-and-triggers).
