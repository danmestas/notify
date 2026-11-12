---
title: Deploy
weight: 1
---

notify runs on [dagnats](../dagnats-sidecar) in one of two shapes:

| Shape | Use it for |
|---|---|
| [Program mode](#program-mode) | notify is its own process, connected to a dagnats server or mesh node. This is how production runs, natively under launchd or systemd or in containers. |
| [Combined binary](#combined-binary) | An application embeds dagnats' server and runs notify in the same process. |

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

### One box with the app on loopback

When the application, notify and dagnats run side by side on one machine, the application usually listens on loopback. notify's [SSRF guard](../security#outbound-requests-ssrf) refuses loopback addresses, so an outbound webhook to the co-located app at `127.0.0.1` is refused by default. Allow that host explicitly:

```sh
NOTIFY_SSRF_ALLOWED_HOSTS=127.0.0.1
```

The value is a comma-separated list of exact hosts. Add only the hosts you need. See the [configuration reference](../../configuration/#serve-core) for the full entry.

### Putting it on the internet

notify binds loopback by default. To expose the API, put a TLS-terminating reverse proxy or a tunnel in front of it and set `NOTIFY_PUBLIC_BASE_URL`. `/healthz`, `/readyz` and `/metrics` are unauthenticated; restrict them at the proxy if they should not be public. Do not expose the dagnats HTTP port: its console has no authentication.

If you receive mail on notify's own SMTP receiver, it must be reachable on port 25 from the internet; see [Reply loop](../../inbound-email/reply-loop#3-the-embedded-smtp-receiver).

## Docker Compose

`compose.yml` is covered in [Docker Compose](../../get-started/docker). Two properties to remember: notify shares the dagnats container's network namespace, and the data lives in two named volumes (`dagnats-data`, `notify-data`) that you must back up.

## Program mode

notify registers with dagnats as a program named `notify` and runs over one connection of its own. Everything in this section is covered in detail by [The dagnats sidecar](../dagnats-sidecar#running-notify-as-a-program); the short version:

- Point `NOTIFY_DAGNATS_URL` at a dagnats server or a node of a dagnats mesh. The default is `nats://127.0.0.1:4222`.
- notify creates its NKey seed on first start (`NOTIFY_SEED`, default `notify.seed` beside the database). The seed is notify's identity on that server: keep it next to `notify.db` and back it up with it.
- Set `NOTIFY_JOIN_KEY` only if the server has a NATS login. A loopback server with no login admits notify without one.
- To move notify to another server or mesh node, change `NOTIFY_DAGNATS_URL` and restart. The same seed registers there as a first registration; [Moving to another dagnats](../dagnats-sidecar#moving-to-another-dagnats) lists what stays behind.

The native and Compose sections below are both program mode: the dagnats server is a separate process and notify connects to it.

## Combined binary

An application can run dagnats' server and notify in one process. It imports `github.com/danmestas/notify/notify` and runs `notify.Program` in-process under the same host that embeds dagnats, instead of starting `notify serve` and dialing a URL. There is no `NOTIFY_DAGNATS_URL` to set and no second process to supervise; you give up running notify and dagnats at different versions or on different machines. `cmd/notify/main.go` shows how `notify.Program` is built and run, and the dagnats documentation covers embedding its server.

## Smoke tests

- `docker/smoke.sh` brings up the Compose stack, waits for health, runs `notify bootstrap` on the shared volume and round-trips an authenticated subscriber upsert. CI runs it.
- `scripts/operator-smoke.sh` exercises the operator journey over HTTP against a running server (`BASE`, `KEY`), with optional SMTP, Cloudflare Email and ntfy legs. It needs `curl` and `jq`.
