# notify

notify is a self-hosted notification and durable-communications engine, written in Go. You define workflows, send them events, and notify delivers through in-app, email, SMS, push and chat channels. It also hosts a subscriber inbox, conversations with agents, a reply loop for inbound email, and a blob store for the original mail. It runs on the [dagnats](https://github.com/danmestas/dagnats) workflow engine.

Docs and overview: <https://notify-docs.daniel-mestas.workers.dev>

- **Workflows** of ordered steps: channel sends, delays, digests, throttles, HTTP requests, streams, and agent messages. Each run is durable and survives restarts.
- **Subscribers and topics**, per-subscriber channel preferences and digest windows, templates with layouts and translations.
- **Inbox and agents**: in-app messages with read state and a live stream, and agents that run on a NATS mesh or on a model notify calls for you.
- **Inbound email**: replies thread back to a conversation, over a webhook, a pipe, or notify's own SMTP receiver.
- **One binary and a SQLite file**, plus the dagnats sidecar. A server-rendered console is built in.
- **A stable HTTP API** under `/v1`. Every response carries a `Notify-Contract` header and the contract only changes additively.

## Quick start

You need `notify` and `dagnats` binaries (see the docs for installing both).

```bash
dagnats serve &                                     # the workflow engine sidecar
notify bootstrap -dev -org acme -env development    # prints an API key, once
notify serve -dev &                                 # API and console on 127.0.0.1:3000
```

`-dev` uses an insecure, publicly known master key. For anything real, set `NOTIFY_MASTER_KEY_HEX` (`openssl rand -hex 32`) instead and keep it.

```bash
export KEY=<the apiKey printed above>

curl -X POST localhost:3000/v1/workflows \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"Order shipped","triggerIdentifier":"order-shipped",
       "steps":[{"stepId":"inbox","type":"in_app",
                 "controlValues":{"subject":"Hi {{subscriber.firstName}}","body":"Order {{payload.orderId}} shipped."}}]}'

curl -X POST localhost:3000/v1/events/trigger \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"order-shipped","to":{"subscriberId":"user-1","firstName":"Ada"},"payload":{"orderId":"A-100"}}'

curl "localhost:3000/v1/messages?subscriberId=user-1" -H "Authorization: ApiKey $KEY"
```

The console is at <http://127.0.0.1:3000/console/>; sign in by pasting the API key.

A Docker Compose file that runs notify with its sidecar is in `compose.yml` (`docker compose up --build`, with `NOTIFY_MASTER_KEY_HEX` set).

## Documentation

The documentation is a Hugo site in [`docs/site`](docs/site), published from this repository. It covers getting started, concepts, workflows and every step type, triggers, channels and providers, the inbox and agents, inbound email and the blob archive, the console, the full API reference, every flag and environment variable, and operations (deploy, the dagnats sidecar, blob storage with Garage, backups).

To read it locally you need Hugo extended (0.168.0 or newer; `brew install hugo`) and Go:

```bash
cd docs/site
hugo server       # live preview on http://localhost:1313
hugo --minify     # build into docs/site/public
```

The first build downloads the Hextra theme as a Hugo module. `docs/site/public/` and `docs/site/resources/` are build output and are not committed.

The site is built and served by Cloudflare Workers Builds from `wrangler.jsonc` (worker `notify-docs`). Its build command installs Hugo, runs `scripts/sync-version.sh` (writes the latest `version-*` tag into `docs/site/hugo.yaml`) and `scripts/gen-llms-txt.sh` (writes `llms.txt` and `llms-full.txt`), then runs `hugo --minify`.

## Development

```bash
go build ./...
go test -race ./...     # the suite starts a dagnats sidecar; set DAGNATS_BIN to a dagnats binary; default ~/projects/notify/.bin/dagnats
```

Tooling is pinned in `mise.toml` (gofumpt, golangci-lint, lefthook); `lefthook.yml` runs formatting, build and lint before each commit, and vet and the race tests before each push. CI is in `.github/workflows`.

Releases are tags named `version-<x.y.z>`; the Release workflow builds `linux/amd64` and `darwin/arm64` binaries.

## License

MIT. See [LICENSE](LICENSE). Third-party notices are in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
