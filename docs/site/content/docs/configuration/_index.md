---
title: Configuration
weight: 11
---

notify is configured with flags and environment variables, interpreted once at start in `cmd/notify/config.go`. For a flag that also has an environment variable, the order is **flag, then environment variable, then default**.

Flags use Go's `flag` package, so `-addr` and `--addr` are the same. Variables marked *env only* have no flag; some are deliberately not flags because a flag value is visible to every user on the machine through `ps`.

## Commands

| Command | Purpose |
|---|---|
| `notify serve` | Run the API server. |
| `notify bootstrap -org <name> -env <name>` | Create an organization, an environment and an API key. Prints `{"apiKey", "orgId", "envId"}` as JSON. Takes `-db`, `-master-key-hex`, `-dev`. Both `-org` and `-env` are required. |
| `notify rotate-key -org <name\|id> -env <name\|id>` | Mint a new key for an existing scope and **revoke every other key** for it. Prints the same JSON. An ambiguous name is an error; pass the id. Takes `-db`, `-master-key-hex`, `-dev`. |
| `notify validate-steps` | Re-validate every stored workflow against the current step rules. Read-only; exits `1` if any fail. Takes `-db`. |
| `notify inbound` | Pipe mode for [inbound email](../inbound-email/reply-loop#2-pipe-mode). Takes `-server`, `-secret`, `-timeout`. |
| `notify version` (also `-v`, `--version`) | Print the version. |

`bootstrap` and `rotate-key` must use the same database and master key as `serve`: the key material is encrypted under it.

## serve: core

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `-addr` | `NOTIFY_ADDR` | `127.0.0.1:3000` | HTTP listen address for the API and console. |
| `-db` | `NOTIFY_DB` | `notify.db` | SQLite database path. Other subcommands read the same variable. |
| `-master-key-hex` | `NOTIFY_MASTER_KEY_HEX` | none | 32 bytes as 64 hex characters. **Required** unless `-dev`. Encrypts API keys, integration credentials, session cookies, environment variables and managed-agent keys. Prefer the environment variable: a flag value is visible in `ps`. |
| `-dev` | `NOTIFY_DEV` (`1` or `true`) | off | Allow starting with no master key by deriving a fixed, **publicly known** one. Development only. |
| `-nats-url` | `NOTIFY_NATS_URL` | `nats://127.0.0.1:4222` | NATS client URL of the running dagnats sidecar. notify does not embed NATS. |
| `-sendmail-command` | `NOTIFY_SENDMAIL_COMMAND` | `/usr/sbin/sendmail -ti` | Command the `sendmail` email provider pipes each message to. Split on whitespace, run without a shell. |
| `-ssrf-allow` | `NOTIFY_SSRF_ALLOWED_HOSTS` | empty | Comma-separated exact hosts that outbound HTTP may reach even though they resolve to an internal address. No wildcards. Empty blocks every internal range. A malformed entry stops start. |
| (env only) | `NOTIFY_PUBLIC_BASE_URL` | empty | The server's public origin. Used to derive the default Web Push VAPID subject (`https://<host>`; localhost and IPs yield none), and passed to the console for rendered webhook URLs. |
| (env only) | `NOTIFY_CONSOLE_READ_ONLY` | off | Exactly `1` disables every mutation in the console. |
| (env only) | `NOTIFY_DAGNATS_CONSOLE_URL` | empty | Base URL of dagnats' console, as your browser sees it, for run deep-links in Activity. |

## serve: agents

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `-mesh-allow` | `NOTIFY_MESH_ALLOW` | `agents.prompt.>` | Allowlist pattern for the NATS subjects mesh agents are dispatched to. |
| `-mesh-nats-url` | `NOTIFY_MESH_NATS_URL` | empty | NATS URL for mesh dispatch. Empty reuses `-nats-url`. Set it when agents live on a different hub from the engine. |

## serve: inbound email

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| (env only) | `NOTIFY_INBOUND_SECRET` | empty | Shared secret the mail forwarder sends as `X-Inbound-Secret`. Empty makes `POST /v1/inbound/email` answer `503`. Also the default of `notify inbound -secret`. |
| (env only) | `NOTIFY_INBOUND_MX_HOST` | empty | The MX host that inbound domains must point at. Empty uses the placeholder `inbound.notify.example.com`. Also the SMTP banner name. |
| `-smtp` | `NOTIFY_SMTP_ADDR` | empty (off) | Listen address for the embedded SMTP receiver, e.g. `0.0.0.0:25`. |
| `-smtp-cert` | `NOTIFY_SMTP_CERT` | empty | PEM certificate for STARTTLS. Reloaded when it changes. Set with `-smtp-key`. |
| `-smtp-key` | `NOTIFY_SMTP_KEY` | empty | PEM private key. Setting one of `-smtp-cert` and `-smtp-key` without the other is an error. |

## serve: blob store

| Environment variable | Default | Meaning |
|---|---|---|
| `NOTIFY_BLOB_DRIVER` | empty (off) | `fs` or `s3`. Anything else stops start. |
| `NOTIFY_BLOB_DIR` | `<directory of NOTIFY_DB>/blobs` | Directory for the `fs` driver. |
| `NOTIFY_BLOB_S3_ENDPOINT` | none | `host:port`, **no scheme**. Required for `s3`. |
| `NOTIFY_BLOB_S3_REGION` | none | Region name. |
| `NOTIFY_BLOB_S3_BUCKET` | none | Bucket. Required for `s3`. notify does not create it. |
| `NOTIFY_BLOB_S3_PREFIX` | none | Key prefix, to share a bucket. |
| `NOTIFY_BLOB_S3_ACCESS_KEY` | none | Access key. |
| `NOTIFY_BLOB_S3_SECRET_KEY` | none | Secret key. |
| `NOTIFY_BLOB_S3_TLS` | off | `1` or `true` to use HTTPS. |

See [Blob storage](../operations/blob-storage).

## serve: chat loop

All env only. See [Chat loop](../inbox-and-agents/chat-loop).

| Environment variable | Default | Meaning |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | empty | Enables the Telegram long-poll. |
| `DISCORD_BOT_TOKEN` | empty | Enables the Discord gateway. |
| `NOTIFY_DISCORD_RESPOND_ALL` | off | `1` or `true`: answer every message in a server channel, not just @-mentions. |
| `NOTIFY_CHAT_DEFAULT_AGENT` | empty | Agent identifier for cold inbound messages. Empty drops them. |
| `NOTIFY_CHAT_DEFAULT_ORG` | empty | Organization id of the default scope. |
| `NOTIFY_CHAT_DEFAULT_ENV` | empty | Environment id of the default scope. |

## AI workflow builder

All env only. The builder is the console's Guided create; it is separate from [managed agents](../inbox-and-agents/managed-agents), which carry their own settings.

| Environment variable | Default | Meaning |
|---|---|---|
| `NOTIFY_AI_API_KEY` | empty | Enables the builder through an OpenAI-compatible API. Takes priority over `ANTHROPIC_API_KEY`. |
| `NOTIFY_AI_BASE_URL` | `https://openrouter.ai/api/v1` | Endpoint for the above. |
| `NOTIFY_AI_MODEL` | `openai/gpt-4.1` | Model for the above. |
| `ANTHROPIC_API_KEY` | empty | Used when `NOTIFY_AI_API_KEY` is unset: Anthropic directly. |
| `NOTIFY_AI_TOKEN_BUDGET` | `300000` | Token cap for one durable build. `0` disables the cap; an unparsable or negative value uses the default. |

With neither key set the builder is disabled.

## serve: telemetry and analytics

| Environment variable | Default | Meaning |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | OTLP endpoint. **Empty disables tracing completely**: nothing is constructed. Setting it also switches log output to structured `slog` text with `trace_id` and `span_id` on records logged inside a span. |
| `OTEL_SERVICE_NAME` | `notify` | Service name on traces. |
| `NOTIFY_DUCKDB` | `notify-analytics.duckdb` | DuckDB file for analytics. Only read by a binary built with `-tags duckdb`; see [Observability](../operations/observability#analytics-backend). |

## notify inbound

| Flag | Environment variable | Default |
|---|---|---|
| `-server` | `NOTIFY_SERVER_URL` | `http://127.0.0.1:3000` |
| `-secret` | `NOTIFY_INBOUND_SECRET` | none (required) |
| `-timeout` | none | `30s` |

## Not read by notify

`deploy/notify-prelude.sh` and `deploy/compose.hub.yml` also mention `RESEND_API_KEY`, `TWILIO_*`, `SIGNALWIRE_*`, `CF_API_TOKEN`, `CF_EMAIL_FROM`, `FIREBASE_*` and `TELEGRAM_CHAT_ID`. **`notify serve` does not read these.** The hub prelude passes them through for operators and logs which are set (names only); providers are configured as [integrations](../channels) through the API.
