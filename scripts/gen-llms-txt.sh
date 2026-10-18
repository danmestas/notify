#!/usr/bin/env bash
# Write llms.txt (a short summary) and llms-full.txt (every docs page,
# concatenated) into docs/site/static so Hugo publishes them at the site root
# for LLM readers. Run by the Cloudflare build before Hugo; the output is
# generated, so it is gitignored.
set -euo pipefail

SITE_DIR="docs/site"
STATIC_DIR="$SITE_DIR/static"

mkdir -p "$STATIC_DIR"

cat > "$STATIC_DIR/llms.txt" << 'HEADER'
# notify

> Self-hosted notification and durable-communications engine, written in Go. Workflows, subscribers, topics, channels (in-app, email, SMS, push, chat), an inbox and agent API, inbound email, and a blob store. It runs on the dagnats workflow engine.

## Key Concepts

- **Scope**: an organization plus an environment. Every API key belongs to one scope and every object is scoped to it.
- **Subscriber**: a recipient, identified by your own `subscriberId`.
- **Workflow**: an ordered list of steps with a trigger identifier. Step types: in_app, email, push, chat, sms, delay, digest, throttle, http_request, stream, agent_message.
- **Trigger**: `POST /v1/events/trigger` runs the workflow named by `name` for a subscriber or a topic.
- **Integration**: stored, encrypted provider credentials for a channel.
- **Agent**: a conversational endpoint on the `mesh` (NATS) or `managed` (notify calls a model) runtime.

## Quick Start

1. `dagnats serve` starts the workflow engine sidecar.
2. `notify bootstrap -org acme -env production` creates a tenant and prints its API key.
3. `notify serve` starts the API on 127.0.0.1:3000 (use `-dev` locally, or set `NOTIFY_MASTER_KEY_HEX`).
4. Create a workflow with `POST /v1/workflows`, then run it with `POST /v1/events/trigger`.

## API

All routes are under `/v1`, authenticated with `Authorization: ApiKey <key>`. Every `/v1` response carries `Notify-Contract: 1`; the contract only changes additively.

## Documentation

Full docs: https://github.com/danmestas/notify/tree/main/docs/site
HEADER

{
    echo "# notify Documentation (Full)"
    echo ""
    echo "Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo ""
    find "$SITE_DIR/content" -name "*.md" -type f | sort | while read -r file; do
        echo "---"
        echo "# Source: $file"
        echo ""
        # Drop the leading YAML frontmatter block only; later "---" lines are
        # horizontal rules in the page body.
        awk 'NR == 1 && $0 == "---" { infm = 1; next }
             infm && $0 == "---" { infm = 0; next }
             !infm { print }' "$file"
        echo ""
    done
} > "$STATIC_DIR/llms-full.txt"

echo "Generated llms.txt and llms-full.txt in $STATIC_DIR/"
