---
title: Security
weight: 6
---

## The master key

`NOTIFY_MASTER_KEY_HEX` is a 32-byte AES-256-GCM key, 64 hex characters. Generate one with `openssl rand -hex 32`.

It is the root of notify's encryption at rest. It seals:

- API key material (the database also stores only a hash of each key for lookup),
- integration credentials,
- environment variable values,
- `http_request` header values,
- stream-source secrets,
- managed-agent API keys,
- console session cookies, and the HMAC key for subscriber session tokens and reply tokens (derived from it).

Consequences:

- **Without `-dev`, notify refuses to start with no key.** An empty key would otherwise mean a fixed, publicly computable one.
- `-dev` derives that fixed key on purpose. Use it only for local development; anything encrypted under it is readable by anyone.
- Every command that opens the database (`serve`, `bootstrap`, `rotate-key`) must use the same key as the database was written with. A different key does not corrupt data but makes it unreadable.
- **There is no key-rotation command.** Treat the master key as long-lived and back it up.

## API keys

`notify bootstrap` mints a key and stores a hash; the raw key is shown once. If you lose it, `notify rotate-key -org … -env …` mints a replacement and **revokes every existing key** for that scope. It exists because the console's regenerate button needs a login, which needs the key you lost. The console can also regenerate a key (Settings, API keys).

Treat an API key as a backend credential. It reads every subscriber in its scope. Give browsers a [subscriber session](../../inbox-and-agents/subscriber-sessions) instead, which is limited to one subscriber's inbox, expires after an hour, and works nowhere else.

## Separate credentials for separate doors

Each unauthenticated-by-key door has its own secret that works nowhere else: `X-Inbound-Secret` for inbound email, `SourceKey` for stream sources, the signing secret for outbound webhooks. None of them can call the management API, and an API key cannot be used on them. Inbound email fails closed: with no `NOTIFY_INBOUND_SECRET` the endpoint answers `503`.

## Outbound requests (SSRF)

Workflow `http_request` steps, webhook providers, ntfy, outbound webhook endpoints and the other clients that call a URL a tenant supplies go through a guard that refuses, after DNS resolution, loopback, link-local, private, unique-local, unspecified, multicast, CGNAT (including Tailscale), `0.0.0.0/8`, NAT64 and broadcast addresses. Checking at dial time defeats DNS rebinding. Open exceptions deliberately with `NOTIFY_SSRF_ALLOWED_HOSTS`: exact hosts only, no wildcards.

## Host configuration that tenants cannot reach

Some settings are host-level on purpose, so that an API-key holder cannot use them to run code or open ports: the sendmail command (`NOTIFY_SENDMAIL_COMMAND`), the SMTP receiver, the SSRF allowlist, blob storage. They are flags and environment variables, never integration fields.

## What notify does not do

- It does not terminate TLS for the API. Put a proxy or tunnel in front.
- `/healthz`, `/readyz` and `/metrics` are unauthenticated.
- The console authenticates with an API key, not users; anyone with the key has the console.
- The embedded SMTP receiver does no DKIM or SPF checks. Its protection is that it accepts only valid, armed reply mailboxes, and refuses everything else before reading the body.
