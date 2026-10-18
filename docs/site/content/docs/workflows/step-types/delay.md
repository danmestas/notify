---
title: Delay
weight: 2
---

A `delay` step pauses the run. It has three forms, chosen by `type` in `controlValues`.

## Regular delay

A fixed wait.

```json
{ "stepId": "wait", "type": "delay", "controlValues": { "amount": 10, "unit": "minutes" } }
```

| Field | Meaning |
|---|---|
| `type` | Absent or `"regular"`. |
| `amount` | A positive integer. `0` or negative is refused. |
| `unit` | `seconds` (the default if omitted), `minutes`, `hours`, `days`, `weeks`, `months`. `months` is a fixed 30 days. |

A new delay step created in the console starts at 1 minute. A regular delay is an engine-native sleep, so no notify worker is held while it waits.

## Scheduled delay

Wait until the next time of day, optionally on certain days.

```json
{ "type": "scheduled", "atTime": "09:00", "weekDays": ["MON", "TUE", "WED", "THU", "FRI"] }
```

| Field | Meaning |
|---|---|
| `type` | `"scheduled"`. |
| `atTime` | Required. `HH:MM`, 24-hour. |
| `weekDays` | Optional. Any of `MON`, `TUE`, `WED`, `THU`, `FRI`, `SAT`, `SUN`. |
| `monthDays` | Optional. Integers 1 to 31. |

Empty `weekDays` and `monthDays` mean every day. If both are given, a day must match both ("the 1st, when it is a Monday").

The time is evaluated in the **subscriber's timezone** (`subscriber.timezone`, an IANA name such as `Europe/Berlin`). A subscriber with no timezone, or one that does not parse, falls back to UTC. Daylight saving changes are handled: 09:00 stays 09:00 local. (Some validation error messages say "UTC"; that is the fallback case.)

With `weekDays` only, `monthDays` only, or neither, the step compiles to an engine-native cron sleep. With both, it runs as a notify task that recomputes the remaining time on every dispatch, so it survives restarts.

## Dynamic delay

Wait for a time that the trigger's payload supplies.

```json
{ "type": "dynamic", "dynamicKey": "payload.sendAt", "dynamicKind": "instant" }
```

| Field | Meaning |
|---|---|
| `type` | `"dynamic"`. |
| `dynamicKey` | Required. A dot path rooted at `payload`, for example `payload.sendAt` or `payload.schedule.at`. |
| `dynamicKind` | Required, and has no default. `"instant"`: the value is an RFC 3339 timestamp, and the run waits until then. `"relative"`: the value is an object `{"amount": 30, "unit": "minutes"}` using the same units as a regular delay, and the run waits that long from when the step starts. |

A step that omits `dynamicKind` is refused at authoring time with `dynamic delay requires a dynamicKind ("instant" or "relative")`. The kind is part of the step, so the same step cannot behave two ways depending on what a trigger happened to send.

The payload value is resolved when the trigger arrives. If the value is missing or does not match the declared kind, the run fails at this step. The `stepId` of a dynamic delay must not contain a dot.

## Notes

- A regular or native delay holds no notify process while waiting; notify can restart freely.
- A delay produces no output. Steps after it re-read the run's context from the database, so templates after a delay still see `subscriber`, `payload` and earlier `steps`.
