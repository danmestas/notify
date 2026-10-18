---
title: Triggers
weight: 4
---

A trigger starts a workflow. There are two ways in, and both end in the same code path.

## POST /v1/events/trigger

```bash
curl -X POST localhost:3000/v1/events/trigger \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{
    "name": "order-shipped",
    "to": {"subscriberId": "user-1", "email": "ada@example.com", "firstName": "Ada"},
    "payload": {"orderId": "A-100"},
    "transactionId": "order-A-100-shipped"
  }'
```

| Field | Meaning |
|---|---|
| `name` | The workflow's `triggerIdentifier`. |
| `to.subscriberId` | Send to one subscriber. `to.email`, `to.firstName`, `to.lastName` and `to.phone` are optional and create or update the subscriber's profile. |
| `to.topicKey` | Send to every member of a [topic](../concepts#topics) instead. Subscriber profiles are not touched; each member's stored profile is used. |
| `payload` | Any JSON. Available to templates as `payload` and to conditions as `payload`. |
| `transactionId` | Optional idempotency key for the send. Minted (a ULID) if absent. |

The response:

```json
{ "acknowledged": true, "status": "processed", "transactionId": "01J…" }
```

| Outcome | HTTP | Body |
|---|---|---|
| Accepted | `201` | `acknowledged: true`, `status: "processed"` |
| No workflow with that name | `404` | `acknowledged: false`, `status: "error"`, `error: ["workflow not found"]` |
| Workflow inactive | `422` | `error: ["workflow is inactive"]` |
| Workflow has no steps | `422` | `error: ["workflow has no steps"]` |
| Unknown topic | `404` | `error: ["topic not found"]` (an `error` entry ending in `not found` maps to 404) |

Acceptance means the run was started, not that delivery succeeded; delivery is asynchronous. Follow it with `GET /v1/notifications?transactionId=…` and `GET /v1/notifications/{id}/messages`.

### Idempotency

The engine admits one run per transaction id and recipient. Repeating a trigger with the same `transactionId` and `to` is a retry and does not start a second run. For a topic trigger, all recipients share the transaction id, and each recipient still gets one run.

### Topic fan-out

A topic trigger starts one run per member, under one transaction id. It records the topic against the transaction so the console's topic activity can find it, even when the topic has no members.

## POST /v1/inbound/{trigger}

A webhook-shaped alternative: the path names the workflow, the body is the same trigger request, and the `name` in the body is ignored.

```bash
curl -X POST localhost:3000/v1/inbound/order-shipped \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"to": {"subscriberId": "user-1"}, "payload": {"orderId": "A-100"}}'
```

It needs the same API key, and answers `201` with the trigger response, or `500` on an internal failure. Use it when the sender can only configure a URL. Note that unlike `/v1/events/trigger` it does not translate a refused trigger into 404 or 422: the status is `201` whenever the call did not error, so read `acknowledged` in the body.

This path is different from `POST /v1/inbound/email`, `POST /v1/inbound/messages` and `/v1/inbound/domains`, which belong to [inbound email](../inbound-email).

## Digests and triggers

If the workflow's first step is a [digest](../workflows/step-types/digest), only the first trigger of a window starts a run; the rest are merged into it and still answer `acknowledged: true`.

## Dynamic delays

A [dynamic delay](../workflows/step-types/delay#dynamic-delay) reads its wake-up time from the payload when the trigger arrives. A value that is missing or malformed makes the run fail at that step.
