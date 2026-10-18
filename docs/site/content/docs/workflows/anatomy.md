---
title: Anatomy of a workflow
weight: 1
---

```json
{
  "name": "Order shipped",
  "triggerIdentifier": "order-shipped",
  "critical": false,
  "steps": [
    { "stepId": "inbox", "type": "in_app", "controlValues": { "subject": "Shipped", "body": "Order {{payload.orderId}} is on its way." } },
    { "stepId": "wait", "type": "delay", "controlValues": { "amount": 10, "unit": "minutes" } },
    { "stepId": "mail", "type": "email", "controlValues": { "subject": "Shipped", "body": "Order {{payload.orderId}} is on its way." } }
  ]
}
```

## Fields

| Field | Meaning |
|---|---|
| `name` | Display name. |
| `triggerIdentifier` | The name you pass as `name` when triggering. Lowercase letters, digits, hyphens and underscores, starting with a letter or digit (`^[a-z0-9][a-z0-9_-]*$`). Unique within an environment. To get a copy under a new identifier, duplicate the workflow. |
| `steps` | The ordered list of steps. A workflow with no steps cannot be triggered. |
| `critical` | When `true`, subscribers cannot opt out of this workflow: their channel preferences do not suppress its delivery. Default `false`. |
| `payloadSchema` | Optional JSON, accepted by `PUT /v1/workflows/{slug}`. Stored with the workflow and with each of its versions. Triggers are not validated against it. |

Each step has:

| Field | Meaning |
|---|---|
| `stepId` | An id unique within the workflow. If you leave it out, the API names it `<type>-<index>`, for example `email-0`. A step id is how later steps and [conditions](../conditions) refer to this step's output, so keep it free of dots. |
| `type` | One of the [step types](../step-types). An unknown type is a `422` that lists the allowed ones. |
| `controlValues` | The step's configuration: a JSON object whose fields depend on the type. |
| `name` | Optional display name. |
| `filters` | Optional [condition](../conditions) that decides whether a channel step runs. Other step types ignore it. |

## Execution order

Steps run strictly in the order listed. Each step starts when the previous one finishes. A step that is skipped (a condition that is false, or a channel the subscriber turned off) counts as finished. A step that fails permanently, such as a [throttle](../step-types/throttle) at its limit or an `http_request` with `failurePolicy: fail_closed`, stops the run; later steps do not execute.

The run is a durable dagnats run. If notify or the sidecar restarts, the run resumes.

## Validation

`POST /v1/workflows`, `PUT /v1/workflows/{slug}` and the console compile the steps into an engine definition before saving. A step whose `controlValues` do not satisfy its type is refused with `422` and the reason, for example `delay amount must be positive, got 0` or `stream: topic is required`. The rules for each type are on its page.

`notify validate-steps` re-checks every stored workflow against the current rules and exits `1` if any fail. Run it before an upgrade that tightens a rule.

## Creating, updating, activating

- `POST /v1/workflows` creates and publishes. `409` if the identifier exists.
- `PUT /v1/workflows/{slug}` is an **idempotent upsert**: it creates the workflow if absent (`201`), returns `200` with `"outcome": "unchanged"` if the submitted definition equals the stored one (no write, no new version), and otherwise applies the update (`"outcome": "updated"`). With `?onConflict=keep`, a differing definition is left alone and answered with `409` and the stored definition. `critical` is optional on update: leaving it out keeps the current value.
- `PATCH /v1/workflows/{slug}/activate` with `{"active": false}` stops accepting triggers. An inactive workflow answers triggers with `acknowledged: false` and `"workflow is inactive"`.
- `POST /v1/workflows/{slug}/duplicate` copies the steps and payload schema to a new `triggerIdentifier`.
- `DELETE /v1/workflows/{slug}` removes the workflow and its engine registration.

Every publish records a **version**, a full snapshot of steps, payload schema and critical flag. The console can roll back to an earlier version. A rollback appends a new latest version, and on restart notify re-registers each workflow's latest published version with the engine. The engine keeps its workflow registry in memory, so this is what keeps workflows firing after a restart.

## Secrets in steps

Fields a step type declares secret are encrypted at rest with the master key and shown masked when read back. Today that is the `headers` field of [`http_request`](../step-types/http-request).
