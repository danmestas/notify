---
title: Throttle
weight: 4
---

A `throttle` step puts a rate limit on a workflow for each subscriber. Under the limit the run goes on. At the limit the run **fails permanently**: no later step executes, and notify records a message with status `throttled` so the suppression is visible in Activity.

```json
{ "stepId": "limit", "type": "throttle", "controlValues": { "limit": 3, "windowSec": 3600 } }
```

| Field | Meaning |
|---|---|
| `limit` | Required, a positive integer: the number of runs allowed per window. The console seeds `10`. |
| `windowSec` | Required, a positive integer: the window in seconds. The console seeds `3600`. |
| `throttleKey` | Optional. A dot path rooted at `payload.`, for example `payload.check`. Gives each distinct value its own budget. Subscriber paths are refused because the budget is already per subscriber. |

The counter is a fixed window keyed by environment, subscriber and workflow (plus the `throttleKey` value when set).

Put a standalone throttle at the start of a workflow to protect everything after it. To limit a single channel without ending the run, use the [inline throttle](../channel-steps#inline-throttle) on that step instead: it records `throttled` for that step and the run continues.
