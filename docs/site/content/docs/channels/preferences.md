---
title: Preferences
weight: 5
---

Subscribers can turn channels off, globally or per workflow, and choose how often a digest arrives. notify checks this before each channel send.

## Channels

The channel keys are `in_app`, `push`, `email`, `sms` and `chat`. A channel is **enabled unless explicitly set to `false`**. A workflow-level setting overrides the global one. A [`critical`](../../workflows/anatomy) workflow ignores preferences entirely.

A disabled channel does not fail the run. The step records a message with status `skipped` and `skipReason: "preference"`.

### SMS default

SMS is the exception to "enabled unless set to `false`". With no SMS preference for the workflow, either per workflow or global, the workflow's own default decides: its sms step's `defaultEnabled`, `false` unless set. So a sign-off workflow sets `"defaultEnabled": true` and texts everyone with a verified phone, while every other workflow texts only subscribers who turned SMS on. An explicit preference always wins, in the usual order (per workflow over global), with the same on/off meaning as email. A critical workflow sends regardless.

`GET` still reports `global.sms` as `true` when nothing is stored, as it does for every channel; read it as "no global preference", since the default then comes from each workflow.

## API

```bash
curl localhost:3000/v1/subscribers/user-1/preferences -H "Authorization: ApiKey $KEY"
```

```json
{
  "global": { "in_app": true, "push": true, "email": false, "sms": true, "chat": true },
  "workflows": { "order-shipped": { "email": true } },
  "digests": { "repo-activity": { "amount": 1, "unit": "hours", "source": "workflow" } }
}
```

- `global` always lists every channel, resolved.
- `workflows` is keyed by **trigger identifier** and lists only the channels explicitly overridden for that workflow. An omitted channel inherits `global`.
- `digests` is keyed the same way and lists the effective window for every workflow that has a regular [digest](../../workflows/step-types/digest) step. `source` is `"subscriber"` when the subscriber has overridden it and `"workflow"` when the step's authored window applies.

`PUT /v1/subscribers/{id}/preferences` takes the same shape. Updates are partial: a channel key you leave out keeps its stored value. Unknown channels, unknown trigger identifiers, and digest settings for a workflow with no regular digest step fail the whole request with `422` before anything is written. `{id}` is your `subscriberId`.

## Digest windows

A subscriber can set their own window per workflow:

```json
{ "digests": { "repo-activity": { "amount": 1, "unit": "days" } } }
```

`amount` must be positive and `unit` one of `seconds`, `minutes`, `hours`, `days`, `weeks`, `months`. A `null` value clears the override. The window is read when a digest window opens, so a change applies to the next window. Scheduled (cron) digests have no per-subscriber window.
