---
title: HTTP request
weight: 5
---

An `http_request` step calls an HTTP endpoint in the middle of a workflow and keeps the response so later steps can use it, in templates or in [conditions](../../conditions).

```json
{
  "stepId": "check",
  "type": "http_request",
  "controlValues": {
    "url": "https://api.example.com/orders/{{payload.orderId}}",
    "method": "GET",
    "headers": "{\"Authorization\": \"Bearer {{payload.token}}\"}",
    "body": "",
    "timeoutSec": 10,
    "failurePolicy": "continue"
  }
}
```

## Fields

| Field | Default | Meaning |
|---|---|---|
| `url` | `""` | Template. After rendering it must not be empty. Values are inserted as sent (not percent-encoded), so a whole URL can come from the payload. |
| `method` | `GET` | `GET`, `POST`, `PUT`, `PATCH` or `DELETE`. |
| `headers` | `""` | A **string** containing a JSON object of header names to values, for example `"{\"X-Key\": \"abc\"}"`. Stored as a string so it fits the flat `controlValues` shape. Values are JSON-escaped when rendered. A header whose name or value contains a line break after rendering is a failure (see Failure policy). **Encrypted at rest** and shown masked when read back. A blank or malformed string means no headers. |
| `body` | `""` | Template. If the body looks like JSON (the `Content-Type` header says so, or it starts like an object or array), inserted values are JSON-escaped; otherwise they are inserted as sent. An empty body sends none. |
| `timeoutSec` | `30` | Per-request deadline, in seconds. Absent or non-positive means 30. At most 300; the editor refuses more, and a larger stored value runs as 300. |
| `failurePolicy` | `fail_closed` | `fail_closed` or `continue`. See below. |

Templates see `subscriber` and `payload`.

## Output

The response is saved for the rest of the run:

```json
{ "statusCode": 200, "body": "…", "headers": { "Content-Type": "application/json" } }
```

`body` is capped at 1 MiB, and the stored header map is capped as well. Later steps read it as `{{steps.check.statusCode}}` and `{{steps.check.body}}`, where `check` is this step's `stepId`; a condition names it with `"on": "check"`. Outputs are kept for 7 days.

## Failure policy

- A **completed exchange is a success**, even a `404` or `500`. The status and body are captured and the run goes on, so a condition can branch on `statusCode`.
- A **transport failure** (connection refused, DNS, the `timeoutSec` deadline, an empty rendered URL) follows `failurePolicy`:
  - `fail_closed`: the run fails permanently, no later step runs, and no output is saved.
  - `continue`: an empty output `{"statusCode": 0, "body": "", "headers": {}}` is saved and the run continues.

## Network restrictions

Requests go through an SSRF-safe client. After DNS resolution it refuses to connect to loopback, link-local, private (RFC 1918, unique-local IPv6), unspecified, multicast, CGNAT (`100.64.0.0/10`, which includes Tailscale), the `0.0.0.0/8` "this network", NAT64 and broadcast addresses. This also stops DNS-rebinding.

If a workflow genuinely needs an internal host, list it in `NOTIFY_SSRF_ALLOWED_HOSTS` (flag `-ssrf-allow`): a comma-separated list of **exact** hosts or IP literals. No wildcards, suffixes or CIDRs; a mistyped entry stops notify at startup. The same guard and allowlist cover the webhook providers, ntfy, outbound webhooks and the other clients that call a tenant-supplied URL.
