---
title: Health and metrics
weight: 9
---

These are not under `/v1`, need no credential, and carry no `Notify-Contract` header. Gate them at your ingress if they should not be public.

| Method and path | Purpose |
|---|---|
| `GET /healthz` | Liveness. `200 {"status": "ok", "version": "…"}`. |
| `GET /readyz` | Readiness. `200 {"status": "ok", "dagnats": true, "db": true}`, or `503` with `"status": "not ready"` and the failing check false. Both checks run live on every request: notify's NATS connection to dagnats, and a ping of the SQLite writer. |
| `GET /metrics` | JSON ([expvar](https://pkg.go.dev/expvar)) of `notify_`-prefixed variables only: `notify_delivery_total` (delivery outcomes by channel), `notify_nats_status`, `notify_nats_disconnects`, `notify_nats_reconnects`. The process's command line and memory statistics are not exposed. |
| `GET /` | `302` to `/console/`. |
| `/console/...` | The [console](../../console). |
