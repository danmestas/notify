---
title: Workflows and triggers
weight: 1
---

All routes take an API key.

| Method and path | Purpose | Success |
|---|---|---|
| `POST /v1/events/trigger` | [Trigger](../../triggers) a workflow for a subscriber or topic. | `201`; `404` or `422` with `acknowledged: false` when refused. |
| `POST /v1/inbound/{trigger}` | Trigger by URL. The path is the workflow, the body is the trigger request. | `201` |
| `POST /v1/workflows` | Create and publish a workflow. | `201` `{id, triggerIdentifier, name, active}` |
| `GET /v1/workflows` | List workflows. | `200` array of `{id, name, triggerIdentifier, active, critical}` |
| `GET /v1/workflows/{slug}` | One workflow in full. `{slug}` is the trigger identifier. | `200` |
| `PUT /v1/workflows/{slug}` | Idempotent upsert. `?onConflict=update` (default) or `keep`. | `201` created, `200` unchanged or updated, `409` on `keep` with a differing definition |
| `DELETE /v1/workflows/{slug}` | Delete. | `204` |
| `PATCH /v1/workflows/{slug}/activate` | Body `{"active": bool}`. | `200` the workflow |
| `POST /v1/workflows/{slug}/duplicate` | Body `{"name", "triggerIdentifier"}`. | `201`; `409` if the identifier is taken |
| `GET /v1/workflow-templates` | List built-in [templates](../../workflows/templates). | `200` |
| `POST /v1/workflow-templates/{name}` | Apply one. | `201` created, `200` already present, `404` unknown |

## Workflow bodies

`POST /v1/workflows`:

```json
{ "name": "Order shipped", "triggerIdentifier": "order-shipped", "critical": false,
  "steps": [ { "stepId": "inbox", "type": "in_app", "controlValues": { "subject": "…", "body": "…" } } ] }
```

`triggerIdentifier` is required (`400`) and must match `^[a-z0-9][a-z0-9_-]*$` (`422`). An unknown step `type` is `422` and lists the allowed ones: `in_app, email, push, chat, sms, delay, digest, throttle, http_request, stream, agent_message`. A step whose `controlValues` fail its type's rules is `422` with the reason. A duplicate identifier is `409`.

`PUT /v1/workflows/{slug}` takes `{name, steps, payloadSchema?, critical?}`; the slug is the identifier.

The detailed workflow (`GET`, `PUT`, `PATCH activate`, template apply) is:

```json
{ "id": "…", "name": "…", "triggerIdentifier": "…", "active": true, "origin": "…",
  "critical": false, "steps": [ … ], "payloadSchema": { … }, "outcome": "created" }
```

`outcome` (`created`, `unchanged` or `updated`) appears on `PUT` and template apply only; `payloadSchema` is omitted when empty. Secret step fields such as `http_request` `headers` are returned masked.

## Trigger body

```json
{ "name": "order-shipped",
  "to": { "subscriberId": "user-1", "email": "", "firstName": "", "lastName": "", "phone": "", "topicKey": "" },
  "payload": { }, "transactionId": "" }
```

Details and the response table are on the [Triggers](../../triggers) page.
