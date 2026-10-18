---
title: Observability
weight: 5
---

## Health

- `GET /healthz`: liveness. `200 {"status":"ok","version":"…"}`. Reports the build version, which makes it a way to see what is actually running.
- `GET /readyz`: readiness. `200` only when the dagnats bridge answers `/ready` **and** the SQLite writer answers a ping; otherwise `503`, with `bridge` and `db` saying which. Both are checked live on each request, with a 3-second timeout each.

Use `/healthz` for a restart decision and `/readyz` to decide whether to send traffic.

## Metrics

`GET /metrics` returns JSON of `notify_`-prefixed expvar variables only:

- `notify_delivery_total`: delivery outcomes by channel.
- `notify_nats_status`, `notify_nats_disconnects`, `notify_nats_reconnects`: the NATS link.

The process's command line and memory statistics are not exposed, because the endpoint is typically public. It is unauthenticated: restrict it at your proxy if you need to.

## Logs

notify logs to standard error. By default the format is the standard `log` package's. Setting `OTEL_EXPORTER_OTLP_ENDPOINT` switches to structured `slog` text with `trace_id` and `span_id` on records logged inside a span.

Things worth alerting on:

- Start-up failures naming the bridge: unreachable, or rejected credentials (`DAGNATS_BRIDGE_TOKEN`).
- `emailloop:` lines. Every inbound mail that is dropped is logged with the address and the reason (`no_token`, `invalid_token`, `domain_unverified`, `no_binding`, …). The loop is never silent.
- `retention:` lines for purge failures.
- `trigger: digestKey resolved to nothing`: a digest that is merging unrelated events.

## Tracing

Set `OTEL_EXPORTER_OTLP_ENDPOINT` (and optionally `OTEL_SERVICE_NAME`, default `notify`) to export OpenTelemetry traces. With it unset, nothing is constructed and the OTel globals are untouched. A trigger is traced from the API call through the engine run to delivery.

## Activity and run inspection

The console's Activity view lists every notification and its messages with live tail. With `NOTIFY_DAGNATS_CONSOLE_URL` set, each run links to the run page in dagnats' own console.

## Analytics backend

By default delivery analytics live in SQLite. A binary built with the `duckdb` tag stores them in an embedded DuckDB file instead (`NOTIFY_DUCKDB`, default `notify-analytics.duckdb`), while all transactional data stays in SQLite. It needs cgo, so the default image and release binaries do not include it:

```bash
CGO_ENABLED=1 go build -tags duckdb -o notify ./cmd/notify
```
