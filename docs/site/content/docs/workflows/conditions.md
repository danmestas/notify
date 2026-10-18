---
title: Step conditions
weight: 4
---

A channel step (`in_app`, `email`, `push`, `chat`, `sms`, `stream`) can carry a `filters` field that decides whether it runs. The other step types do not evaluate `filters`. A step whose condition is false is **skipped**: a channel step records a message with status `skipped` and `skipReason: "condition"`, and the run goes on with the next step. A step with no `filters`, or an empty array, always runs.

```json
{
  "stepId": "sms-vip",
  "type": "sms",
  "controlValues": { "body": "Your order shipped." },
  "filters": [
    {
      "combinator": "AND",
      "conditions": [
        { "on": "subscriber", "field": "data.plan", "operator": "EQUAL", "value": "vip" },
        { "on": "payload",    "field": "amount",    "operator": "LARGER", "value": 100 }
      ]
    }
  ]
}
```

## Structure

`filters` is an array of **groups**. The step runs if **any** group passes (the top-level array is OR).

A group has:

| Field | Meaning |
|---|---|
| `combinator` | `"AND"` (the default) or `"OR"`, case-insensitive. Anything other than `OR` is `AND`. |
| `conditions` | Leaf tests. |
| `groups` | Nested groups, combined with the same combinator. |

A group with no conditions and no nested groups passes.

A condition has:

| Field | Meaning |
|---|---|
| `on` | The data source: `subscriber`, `payload`, or the `stepId` of an earlier [`http_request`](../step-types/http-request) or [`agent_message`](../step-types/agent-message) step (its saved output). |
| `field` | A dot path inside the source, for example `data.plan` or `statusCode`. |
| `operator` | One of the operators below. |
| `value` | The comparand. |
| `negated` | `true` inverts the result. |

## Operators

| Operator | Passes when |
|---|---|
| `EQUAL` | The field is present and equals `value`: the string forms match, or both are numbers and numerically equal. |
| `NOT_EQUAL` | The opposite of `EQUAL`. A missing field counts as not equal. |
| `LARGER`, `SMALLER`, `LARGER_EQUAL`, `SMALLER_EQUAL` | Both sides are numbers (or numeric strings) and the comparison holds. |
| `BETWEEN` | The field is a number within `[min, max]` inclusive. `value` must be a two-element list. |
| `NOT_BETWEEN` | The field is a number below `min` or above `max`. Same `value` shape; an invalid shape is false. |
| `IN` | The field is present and equals one element of `value`, which is a list or a comma-separated string. |
| `NOT_IN` | The field is not in `value`. |
| `IS_DEFINED` | The field exists and is not null. |
| `LIKE` | Both are strings and the field **contains** `value`. Case-sensitive substring test. |
| `NOT_LIKE` | Both are strings and the field does not contain `value`. A non-string is not a match. |
| `ANY_IN` | The field is a list and at least one element is in `value`. |
| `ALL_IN` | The field is a list and every element is in `value`. An empty list passes. |

An unknown operator never matches.

## Fail closed

If `filters` is not valid JSON, or a condition cannot be evaluated, the step is **skipped** and the message gets `skipReason: "condition_invalid"`. notify prefers sending nothing to sending to everyone because a condition was broken. `condition_invalid` is distinct from `condition` so a broken filter is visible in Activity and does not look like a filter working as intended.

## Branching on an HTTP call

Because `http_request` saves its response, a condition can use it:

```json
{ "on": "check", "field": "statusCode", "operator": "EQUAL", "value": 200 }
```
