---
title: Deploy
weight: 1
---

There are three ways to run notify. They all run the same two processes, notify and the [dagnats sidecar](../dagnats-sidecar).

| | Use it for |
|---|---|
| [Native](#native-launchd-or-systemd) | A single machine you control. The reference deployment: notify and dagnats as supervised processes on a Mac mini under launchd. |
| [Docker Compose](../../get-started/docker) | Trying it out, CI, or a host where you prefer containers. |
| [The hub compose file](#the-hub-compose-file) | A public-facing deployment that fronts notify with a Cloudflare tunnel and leafs dagnats into a NATS hub. |

## Native (launchd or systemd)

The reference deployment does **not** use Docker or Compose. It runs notify as a native process under launchd, next to a native dagnats sidecar, on the same machine. That is the simplest topology: both bind loopback and nothing needs a shared network namespace.

1. Install the `notify` binary from a [release](../releases) and the `dagnats` binary at the version the repository pins (`DAGNATS_VERSION` in the `Dockerfile`).
2. Start dagnats first, as its own supervised job, with its data directory fixed (`DAGNATS_DATA_DIR`). Keep the binary at a stable path so a restart picks up the intended version.
3. Create the tenant once, with the production database and key: `notify bootstrap -org … -env …`.
4. Run `notify serve` under the supervisor with `KeepAlive`/`Restart=always`.

An example launchd job for notify (adapt paths; secrets belong in your secret store, not in the plist):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>              <string>com.example.notify</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/notify</string>
        <string>serve</string>
    </array>
    <key>RunAtLoad</key>          <true/>
    <key>KeepAlive</key>          <true/>
    <key>ThrottleInterval</key>   <integer>10</integer>
    <key>WorkingDirectory</key>   <string>/var/lib/notify</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>NOTIFY_ADDR</key>            <string>127.0.0.1:3000</string>
        <key>NOTIFY_DB</key>              <string>/var/lib/notify/notify.db</string>
        <key>NOTIFY_PUBLIC_BASE_URL</key> <string>https://notify.example.com</string>
        <!-- NOTIFY_MASTER_KEY_HEX, NOTIFY_INBOUND_SECRET: inject from your secret store -->
    </dict>
    <key>StandardOutPath</key>    <string>/var/log/notify.log</string>
    <key>StandardErrorPath</key>  <string>/var/log/notify.log</string>
</dict>
</plist>
```

On systemd the equivalent is a service with `Restart=always`, `After=` the dagnats unit, and `EnvironmentFile=` pointing at a root-only file.

### Start-up order

Because the supervisor starts jobs independently, notify may start before the sidecar's NATS port is listening, for example after a reboot. notify then exits at start and the supervisor restarts it until dagnats is up. `/readyz` reports `"dagnats": false` whenever notify's connection to dagnats is down.

### Putting it on the internet

notify binds loopback by default. To expose the API, put a TLS-terminating reverse proxy or a tunnel in front of it and set `NOTIFY_PUBLIC_BASE_URL`. `/healthz`, `/readyz` and `/metrics` are unauthenticated; restrict them at the proxy if they should not be public. Do not expose the dagnats HTTP port: its console has no authentication.

If you receive mail on notify's own SMTP receiver, it must be reachable on port 25 from the internet; see [Reply loop](../../inbound-email/reply-loop#3-the-embedded-smtp-receiver).

## Docker Compose

`compose.yml` is covered in [Docker Compose](../../get-started/docker). Two properties to remember: notify shares the dagnats container's network namespace, and the data lives in two named volumes (`dagnats-data`, `notify-data`) that you must back up.

## The hub compose file

`deploy/compose.hub.yml` (project `notify-hub`) is the public-facing variant. It runs three services in one shared network namespace owned by `dagnats`:

- `dagnats`, publishing `127.0.0.1:3000` (for notify) and `127.0.0.1:4222` (the NATS port, for agents running on the host).
- `notify`, built from the `hub` Docker target, whose entrypoint is `deploy/notify-prelude.sh`.
- `cloudflared`, which runs a named Cloudflare tunnel (`TUNNEL_TOKEN`) to `127.0.0.1:3000`.

Setting `DAGNATS_LEAF_REMOTES` (from `SESH_ROOT_LEAF_URL`) puts dagnats in **leaf mode**: it dials out to a root NATS hub, binds NATS on `0.0.0.0:4222` instead of loopback, and authenticates with the credentials file named by `DAGNATS_LEAF_CREDENTIALS` if the root uses one. Because the local dagnats then is both the engine and the agents' hub, `NOTIFY_MESH_NATS_URL` is left empty so mesh dispatch reuses the engine NATS.

The prelude `notify-prelude.sh` runs before `notify serve`. It applies the same defaults notify uses, checks that `NOTIFY_MASTER_KEY_HEX` is 64 hex characters (and warns loudly when it is empty), turns a mounted Firebase service-account file into `FIREBASE_SERVICE_ACCOUNT_B64`, logs which provider credentials are present (names only, never values), warns when a chat bot token is set with no `NOTIFY_CHAT_DEFAULT_AGENT`, and then `exec`s `notify serve`.

Secrets come from the process environment: `doppler run -p notify -c dev -- docker compose -f deploy/compose.hub.yml up -d`. `deploy/stage-creds.sh` pushes them into a scion hub secret store instead, and `deploy/scion-agent.yaml` is the matching agent template. `deploy/root-hub/` holds the root hub's NATS configuration (Tailscale-private, or public TLS with nsc-issued credentials). These are specific to one operator's mesh; most deployments do not need them.

## Smoke tests

- `docker/smoke.sh` brings up the Compose stack, waits for health, runs `notify bootstrap` on the shared volume and round-trips an authenticated subscriber upsert. CI runs it.
- `scripts/operator-smoke.sh` exercises the operator journey over HTTP against a running server (`BASE`, `KEY`), with optional SMTP, Cloudflare Email and ntfy legs. It needs `curl` and `jq`.
