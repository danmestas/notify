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
{ "acknowledged": true, "status": "processed", "transactionId": "01J…",
  "notificationIds": ["01J…"] }
```

`notificationIds` lists the notification each recipient got, one id per recipient in recipient order (a topic trigger lists its members in member order). Pass an id straight to `GET /v1/notifications/{id}/messages` to follow delivery. It is absent when the trigger was refused. A recipient whose event is merged into an open [digest](../workflows/step-types/digest) window has no notification of its own, so has no id.

| Outcome | HTTP | Body |
|---|---|---|
| Accepted | `201` | `acknowledged: true`, `status: "processed"` |
| No workflow with that name | `404` | `acknowledged: false`, `status: "error"`, `error: ["workflow not found"]` |
| Workflow inactive | `422` | `error: ["workflow is inactive"]` |
| Workflow has no steps | `422` | `error: ["workflow has no steps"]` |
| Unknown topic | `404` | `error: ["topic not found"]` (an `error` entry ending in `not found` maps to 404) |

Acceptance means the run was started, not that delivery succeeded; delivery is asynchronous. Follow it with `GET /v1/notifications/{id}/messages` using an id from `notificationIds`; `GET /v1/notifications?transactionId=…` finds the same rows.

### Idempotency

The engine admits one run per transaction id and recipient. Repeating a trigger with the same `transactionId` and `to` is a retry and does not start a second run. For a topic trigger, all recipients share the transaction id, and each recipient still gets one run.

A retry is answered like the first call: the notification rows from the first attempt are reused rather than rewritten, so `notificationIds` returns the same ids.

### Topic fan-out

A topic trigger starts one run per member, under one transaction id. It records the topic against the transaction so the console's topic activity can find it, even when the topic has no members.

## Cancelling a trigger

A workflow that waits before it sends, such as "follow up on the estimate in 3 days", can be stopped by its `transactionId` before it sends:

```bash
curl -X POST localhost:3000/v1/events/trigger/order-A-100-shipped/cancel \
  -H "Authorization: ApiKey $KEY"
```

```json
{ "cancelled": ["01J…"], "alreadyFinished": [] }
```

Every notification of that transaction in the key's environment is cancelled, so a topic trigger stops every member's run. Each notification lands in one list:

| List | Meaning |
|---|---|
| `cancelled` | The run is cancelled and the notification's `Status` is now `cancelled`. Its pending delay or digest window no longer advances it, and none of its remaining steps run. A notification whose run never started is cancelled too, and retrying the trigger afterwards starts nothing. |
| `alreadyFinished` | The run had already completed or failed. Its notification and messages are left as they were. |

| Outcome | HTTP |
|---|---|
| Cancelled, or already finished | `200` with both lists, either of which may be empty |
| No notification with that `transactionId` in this environment | `404` |

Cancelling is idempotent: a second call answers `200` with the same notifications under `cancelled`.

A step already running when the cancel arrives is told to stop, but a send it has already handed to a provider can still go out. Cancelling stops what has not started yet; it does not recall a message.

If you would rather not keep the transaction id, or the decision depends on state you hold, re-check before sending instead: an `http_request` step calls your service, and the send step carries a condition on its response, such as `{"on": "check", "field": "statusCode", "operator": "EQUAL", "value": 200}`. A false condition skips the send with `skipReason: "condition"`. See [Branching on an HTTP call](../workflows/conditions#branching-on-an-http-call). That pattern costs a call to your service per run, and the run still waits out its delay.

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
