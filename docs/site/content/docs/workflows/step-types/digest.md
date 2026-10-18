---
title: Digest
weight: 3
---

A `digest` step batches events. Many triggers for the same subscriber in a window collapse into **one** run, whose later steps can say how many events were merged and list them.

```json
{
  "name": "Repository activity",
  "triggerIdentifier": "repo-activity",
  "steps": [
    { "stepId": "batch", "type": "digest",
      "controlValues": { "type": "regular", "amount": 1, "unit": "hours", "digestKey": "payload.repo" } },
    { "stepId": "mail", "type": "email",
      "controlValues": { "subject": "{{digest.count}} updates", "body": "{{#each digest.events}}- {{this.title}}\n{{/each}}" } }
  ]
}
```

## Fields

| Field | Meaning |
|---|---|
| `type` | `"regular"` (the default when absent) or `"scheduled"`. |
| `amount`, `unit` | Regular only. The window length. `amount` must be a positive integer; `unit` is `seconds`, `minutes`, `hours`, `days`, `weeks` or `months` (30 days). The console seeds 1 minute. |
| `cron` | Scheduled only. A standard **5-field** cron expression, for example `0 9 * * MON`. Seconds and `@hourly`-style descriptors are refused. |
| `digestKey` | Optional. A dot path rooted at `payload.` or `subscriber.` (for example `payload.repo` or `subscriber.data.plan`). Events with different values of the key batch separately. An unrooted path such as `repo` is refused, because it would resolve to nothing and merge every event into one digest. |

## How it works

The digest must be the **first step** of the workflow; only then does the trigger do any merging.

1. A trigger arrives. notify appends the event to the window for this environment, subscriber, workflow and `digestKey` value.
2. The first trigger of a window claims it and starts a run. Later triggers in the window are appended and start nothing; the API still answers `acknowledged: true`.
3. The run waits out the window, then the next step drains the collected events and renders with them.

In templates after the digest:

| Variable | Meaning |
|---|---|
| `{{digest.count}}` | How many events were merged, including non-object payloads. |
| `{{digest.events}}` | The event payloads, as a list, each an object. Non-object payloads are counted but not listed. Use `{{#each digest.events}}{{this.field}}{{/each}}`. |

The run's own `payload` is the first event's payload.

## Regular and scheduled windows

A **regular** window closes at the time of the first trigger plus the window. It compiles to an engine-native sleep, so it holds no notify worker. Each subscriber can override the window for a workflow ([preferences](../../../channels/preferences#digest-windows)); the override is applied when the window opens.

A **scheduled** window closes at the next tick of the cron expression, evaluated by notify at dispatch. A scheduled digest runs as a task that holds a notify worker for the whole window, so many long scheduled digests in flight cost a goroutine each. A shutdown requeues the task rather than dropping the digest.
