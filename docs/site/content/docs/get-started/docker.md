---
title: Docker Compose
weight: 3
---

The repository's `compose.yml` runs the pair that notify needs: a dagnats sidecar and the notify server, with a volume for each.

```bash
export NOTIFY_MASTER_KEY_HEX=$(openssl rand -hex 32)   # keep this; see below
docker compose up --build
```

## What it starts

| Service | Image target | Role |
|---|---|---|
| `dagnats` | `--target dagnats` | The workflow engine sidecar. `DAGNATS_HTTP_ADDR=0.0.0.0:8080`, `DAGNATS_DATA_DIR=/data`, volume `dagnats-data`. |
| `notify` | `--target notify` | The API server. `NOTIFY_DB=/data/notify.db`, volume `notify-data`. |

**notify shares dagnats' network namespace** (`network_mode: "service:dagnats"`). In standalone mode dagnats binds its NATS port to loopback only, so notify reaches it as `nats://127.0.0.1:4222`. Because they share a namespace, the host port for notify is published on the `dagnats` service.

Published ports:

- `${NOTIFY_HOST_PORT:-3000}:3000`: the notify API and console. Change `NOTIFY_HOST_PORT` if 3000 is taken.
- `127.0.0.1:${DAGNATS_CONSOLE_HOST_PORT:-8081}:8080`: dagnats' own console. It has no authentication, so it is bound to loopback on purpose. notify links to it from run pages through `NOTIFY_DAGNATS_CONSOLE_URL`.

The `notify` service sets `NOTIFY_DAGNATS_URL`, `NOTIFY_ADDR=0.0.0.0:3000`, `NOTIFY_DB`, `NOTIFY_DAGNATS_CONSOLE_URL` and `NOTIFY_MASTER_KEY_HEX`. Add any other [environment variable](../../configuration) to the `environment:` block.

Both services have health checks (`/ready` on dagnats, `/healthz` on notify), and notify waits for dagnats to be healthy.

## The master key

`NOTIFY_MASTER_KEY_HEX` is 64 hex characters (32 bytes). It encrypts API keys, integration credentials, session cookies and environment variables at rest. If it is empty, `compose.yml` passes an empty value and notify refuses to start (only `-dev` derives a fixed key). **Losing or changing the key makes the data in `notify.db` unreadable.** Store it with your other secrets.

## Create a tenant

`notify bootstrap` and `serve` must use the same database and key. Run bootstrap inside the running container:

```bash
docker compose exec notify notify bootstrap -org acme -env production
```

Then continue with [step 4 of the quickstart](../quickstart#4-create-a-workflow), against `http://localhost:3000`.

## The image

The `Dockerfile` has these targets:

| Target | Contents |
|---|---|
| `notify` | Static `notify` binary on Alpine 3.20, runs as a non-root `notify` user under `tini`. `EXPOSE 3000`, `VOLUME /data`, default command `serve`. |
| `dagnats` | The dagnats binary, built with `go install` at `DAGNATS_VERSION` (default `v0.0.45`). `EXPOSE 4222 8080`. |
| `hub` | The `notify` target with `deploy/notify-prelude.sh` as entrypoint; see [Deploy](../../operations/deploy#the-hub-compose-file). |

The default image is built with `CGO_ENABLED=0` and does not include the DuckDB analytics backend.

Docker is one way to run notify. The reference deployment runs it as a native process under launchd; see [Deploy](../../operations/deploy).
