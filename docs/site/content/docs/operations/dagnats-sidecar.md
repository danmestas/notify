---
title: The dagnats sidecar
weight: 2
---

[dagnats](https://github.com/danmestas/dagnats) is the workflow engine notify runs on. In a notify deployment it is a **separate process**, the sidecar. notify connects to it; it never spawns it.

## What it provides

- The **durable workflow engine**: notify registers each workflow as a DAG definition, starts a run per trigger, and the engine dispatches the steps.
- An **embedded NATS server with JetStream**. notify uses it for the event streams below, the digest and throttle counters, and mesh agent dispatch (unless `NOTIFY_MESH_NATS_URL` points elsewhere).
- An **HTTP worker bridge**. notify's step handlers (send an email, call a URL, run an agent turn) poll the bridge for tasks, run them in the notify process, and report results back.

notify makes three connections: NATS (`-nats-url`), the bridge (`-bridge-url`), and, on its own, a NATS connection for mesh dispatch.

## Defaults

`dagnats serve` binds NATS on `127.0.0.1:4222` and its HTTP control plane and bridge on `127.0.0.1:8080`, which are notify's defaults. In standalone mode NATS binds **loopback only**, so notify must be on the same host (or share a network namespace, as the Compose setup does). Leaf or cluster mode binds `0.0.0.0`. The sidecar's settings are `DAGNATS_*` variables: `DAGNATS_HTTP_ADDR`, `DAGNATS_DATA_DIR`, `DAGNATS_NATS_PORT`, `DAGNATS_LEAF_REMOTES` and `DAGNATS_LEAF_CREDENTIALS` appear in this repository's deploy files; the dagnats documentation has the rest.

## Authentication

If the sidecar is started with `DAGNATS_BRIDGE_TOKEN`, its bridge requires `Authorization: Bearer <token>` on every call. Give notify the same value as `DAGNATS_BRIDGE_TOKEN` in *its* environment (the name is the same on both sides).

dagnats serves `/ready` outside its authentication, so a bridge that refuses every one of notify's task polls still looks healthy. For that reason notify probes the bridge's credentials at start, using a deliberately invalid request that distinguishes "authenticated, bad body" (`400`) from "refused" (`401`/`403`), and **exits** if it is refused. It also logs, at start, whether bridge auth is enabled. A missing token against a bridge that requires one is the failure this prevents: every task would be rejected and no step would ever run.

## What notify keeps on it

| Name | Purpose | Retention |
|---|---|---|
| Workflow definitions | The compiled DAGs. The engine holds them **in memory**, so notify re-registers every persisted workflow at each start (the latest published version of each). A workflow that fails to compile is logged and skipped. | until restart |
| `NOTIFY_DELIVERY` stream | Delivery-status events; feeds webhooks and the live activity view. | 7 days, file storage |
| `NOTIFY_ANALYTICS` stream | Delivery events for analytics. | |
| `NOTIFY_FEED` stream | The durable [feed](../../streams-and-webhooks#the-feed). | 7 days and a 1 GiB cap across all tenants; oldest evicted first. Producer-supplied event ids are deduplicated for 2 minutes. |

These streams and a few KV buckets live in the sidecar's data directory, so **the sidecar's data directory is state worth keeping**, though the source of truth for your configuration is notify's SQLite file.

## Versions

notify tracks the latest dagnats. The `Dockerfile` (`DAGNATS_VERSION`) and CI pin one release, currently `v0.0.30`, so builds are reproducible rather than following a moving branch. Run the same version in production and bump the pin when you upgrade. Keep the sidecar binary at a stable path.

## Running two environments on one machine

Each notify needs its own sidecar (or at least its own NATS and bridge address), and its own SQLite file. Point `-nats-url` and `-bridge-url` at the right one.
