---
title: Console
weight: 9
---

The operator console is a server-rendered web UI mounted on the same listener at `/console`. The root path `/` redirects to it. It reads and writes through the same services as the API, in process.

## Signing in

Open `/console/login` and paste an API key. The key is sealed (AES-256-GCM, under the master key) into a session cookie, scoped to `/console` so it never travels on `/v1` calls, valid for one week. The key is re-checked on **every request**, so rotating or revoking it ends console sessions immediately. Non-`GET` requests carry a per-session CSRF token. `POST /console/logout` ends the session.

The console shows the scope of the key you used. Create further environments under Settings, Environments.

Set `NOTIFY_CONSOLE_READ_ONLY=1` to disable every mutation button.

## Pages

| Page | What it does |
|---|---|
| Dashboard | Summary KPIs. |
| Workflows | List, create, edit steps, reorder, activate, duplicate, delete; version history and rollback; a test-fire page; per-workflow activity. A **Guided create** tab builds a workflow from a prompt when an AI key is configured ([below](#ai-workflow-builder)). |
| Activity | Every notification and its messages, with live tail, run deep-links to the dagnats console (`NOTIFY_DAGNATS_CONSOLE_URL`), and resend. |
| Analytics | Delivery counts by channel and status, and a time series. |
| Integrations | Add, edit, activate, delete, verify credentials, send a test message, set the Web Push subject. Shows per-integration activity. |
| Subscribers | List, create, edit, delete; channel addresses; preferences. |
| Topics | Topic membership and activity. |
| Webhooks | Outbound endpoints and their secrets. |
| Stream sources | Sources that push to the [feed](../streams-and-webhooks#stream-sources): create, rotate, limits. |
| Agents | Agents, their channel links and webhook URLs, conversations and transcripts, with a send-message form. |
| Chat | Read-only status of the Telegram and Discord loop. |
| Inbound domains / Inbound email | Domains and MX verification; the email thread log and endpoint status. |
| Layouts, Translations | Email layouts and locale maps. |
| Contexts, Variables | Contexts and environment variables. |
| Settings | Environments; API keys (regenerate). |

## AI workflow builder

With `NOTIFY_AI_API_KEY` (an OpenAI-compatible provider, OpenRouter by default) or `ANTHROPIC_API_KEY`, the console can draft and refine a workflow from a prompt, running the model as a durable agent loop on the engine. Without a key the Guided create tab says so and everything else works. See [Configuration](../configuration#ai-workflow-builder).
