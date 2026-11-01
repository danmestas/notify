---
title: The dagnats sidecar
weight: 2
---

[dagnats](https://github.com/danmestas/dagnats) is the workflow engine notify runs on. In a notify deployment it is a **separate process**, the sidecar. notify connects to it; it never spawns it.

## What it provides

- The **durable workflow engine**: notify registers each workflow as a DAG definition, starts a run per trigger, and the engine dispatches the steps.
- An **embedded NATS server with JetStream**. notify uses it for the event streams below, the digest and throttle counters, and mesh agent dispatch (unless `NOTIFY_MESH_NATS_URL` points elsewhere).
- A **NATS control plane and task queues**. notify registers workflows and starts runs with request/reply on dagnats' routes scoped to the notify program (`api.p.notify.workflows.register`, `api.p.notify.runs.start` and `api.p.notify.runs.get`, dagnats v0.0.42 or later), and its step handlers (send an email, call a URL, run an agent turn) claim tasks with dagnats' worker SDK, run them in the notify process, and report results back over the same connection. Up to 256 tasks of one type run at once, so one slow digest wait or agent turn never holds up the rest of its type. While a handler runs, dagnats keeps its task claimed for up to an hour; `notify.agent.turn.managed` and `notify.http_request` set their own caps from their limits. Waits (delays and digests) do not run in a handler at all: they park in dagnats until their time comes.

Every task type and workflow name notify registers starts with `notify.`: `notify.send.email`, `notify.send.chat`, `notify.delay.scheduled`, `notify.digest.wait`, `notify.check.throttle`, `notify.http_request`, `notify.agent_message`, `notify.verify.credentials`, `notify.agent.turn.mesh`, `notify.agent.turn.managed` and `notify.ai.build`. Workflows are `notify.<environment>-<trigger>` for yours, `notify.agent-turn-<agent id>` for agent turns, and `notify.credential-verify` and `notify.ai-builder`. A worker token for another dagnats program therefore cannot claim notify's tasks. For one release, notify still handles the old names (`send.email`, `agent.turn.mesh` and so on) so runs started before the upgrade finish; workflow definitions saved before the upgrade are read with the prefix added.

notify makes two connections: NATS (`-nats-url`) for the engine, and, on its own, a NATS connection for mesh dispatch.

## Defaults

`dagnats serve` binds NATS on `127.0.0.1:4222` and its HTTP console on `127.0.0.1:8080`. notify needs only the NATS address, which is its default. In standalone mode NATS binds **loopback only**, so notify must be on the same host (or share a network namespace, as the Compose setup does). Leaf or cluster mode binds `0.0.0.0`. The sidecar's settings are `DAGNATS_*` variables: `DAGNATS_HTTP_ADDR`, `DAGNATS_DATA_DIR`, `DAGNATS_NATS_PORT`, `DAGNATS_LEAF_REMOTES` and `DAGNATS_LEAF_CREDENTIALS` appear in this repository's deploy files; the dagnats documentation has the rest.

## Agent-loop state

The AI builder keeps its conversation in agent-loop state, which dagnats carries in each task and in the run snapshot.

- dagnats caps the state at `loop_state_max_bytes`, which defaults to 256 KiB.
- Run the sidecar with **`DAGNATS_LOOP_STATE_MAX_BYTES=524288`** (512 KiB). The Docker image sets it, and notify's tests use the same value. A host that runs dagnats natively must set it itself.
- The state travels base64-encoded inside JSON, so the bytes on the wire are about four thirds of the cap: 512 KiB of state is about 683 KiB in each task and snapshot. That has to fit under NATS' `max_payload` (1 MiB by default), so do not raise the cap much further without raising `max_payload` too.
- A build whose conversation outgrows the cap ends with an error that names `DAGNATS_LOOP_STATE_MAX_BYTES`. The error is shown in the builder and logged as `aibuilder: continue failed`.

Before this release the state lived in a KV bucket and could reach about 1 MiB.

## What notify keeps on it

| Name | Purpose | Retention |
|---|---|---|
| Workflow definitions | The compiled DAGs. The engine holds them **in memory**, so notify re-registers every persisted workflow at each start (the latest published version of each). A workflow that fails to compile is logged and skipped. | until restart |
| `NOTIFY_DELIVERY` stream | Delivery-status events; feeds webhooks and the live activity view. | 7 days, file storage |
| `NOTIFY_ANALYTICS` stream | Delivery events for analytics. | |
| `NOTIFY_FEED` stream | The durable [feed](../../streams-and-webhooks#the-feed). | 7 days and a 1 GiB cap across all tenants; oldest evicted first. Producer-supplied event ids are deduplicated for 2 minutes. |

These streams and a few KV buckets live in the sidecar's data directory, so **the sidecar's data directory is state worth keeping**, though the source of truth for your configuration is notify's SQLite file.

## Versions

notify tracks the latest dagnats. The `Dockerfile` (`DAGNATS_VERSION`) and `go.mod` pin one release, currently `v0.0.42`, so builds are reproducible rather than following a moving branch. Run the same version in production and bump the pin when you upgrade. Keep the sidecar binary at a stable path.

## Running two environments on one machine

Each notify needs its own sidecar (or at least its own NATS address), and its own SQLite file. Point `-nats-url` at the right one.
