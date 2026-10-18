---
title: API reference
weight: 10
---

Every route notify serves, from `internal/httpapi` and the services it mounts. The pages in this section list each route with its purpose and the details that are easy to get wrong. The behaviour behind them is explained in the other sections.

{{< cards cols="2" >}}
  {{< card link="workflows-and-triggers" title="Workflows and triggers" subtitle="/v1/workflows, /v1/events/trigger, /v1/workflow-templates, /v1/inbound/{trigger}" >}}
  {{< card link="subscribers-and-topics" title="Subscribers and topics" subtitle="/v1/subscribers, /v1/topics, preferences, credentials, sessions" >}}
  {{< card link="integrations" title="Integrations" subtitle="/v1/integrations, Web Push keys" >}}
  {{< card link="notifications-and-messages" title="Notifications and messages" subtitle="/v1/notifications, /v1/messages, live inbox stream" >}}
  {{< card link="agents" title="Agents and conversations" subtitle="/v1/agents, /v1/inbox/conversations, /v1/inbox/drafts" >}}
  {{< card link="inbound" title="Inbound email" subtitle="/v1/inbound/email, domains, archive" >}}
  {{< card link="streams-and-webhooks" title="Streams and webhooks" subtitle="/v1/feed/stream, /v1/sources, /v1/webhooks/endpoints" >}}
  {{< card link="resources" title="Other resources" subtitle="Layouts, translations, contexts, env vars, analytics" >}}
  {{< card link="system" title="Health and metrics" subtitle="/healthz, /readyz, /metrics" >}}
{{< /cards >}}

## Conventions

### Base path and contract

All API routes are under `/v1`. **Every `/v1` response, errors included, carries `Notify-Contract: 1`.** A `/v1` route only ever changes additively; a change that would break a client moves to a new major path and a new header value, so a client can refuse a server whose contract it does not understand.

### Authentication

| Scheme | Header | Routes |
|---|---|---|
| API key | `Authorization: ApiKey <key>` | Everything under `/v1` unless noted. |
| Subscriber session | `Authorization: Bearer <token>` | The four inbox message routes only (an API key also works there). |
| Source key | `Authorization: SourceKey <key>` | `POST /v1/sources/{sourceID}/events`. |
| Inbound secret | `X-Inbound-Secret: <secret>` | `POST /v1/inbound/email`. |

A missing or unrecognised credential is `401` with a plain-text `unauthorized` body. The key decides the scope; there is no organization or environment parameter on any route.

### Optional routes

Some route groups are mounted only when their backing service is wired. `notify serve` wires all of them, with one exception: the three `/v1/inbound/messages` archive routes exist only when a [blob store](../operations/blob-storage) is configured.

### Errors

Most routes return a JSON envelope:

```json
{ "error": { "code": "not_found", "message": "workflow not found" } }
```

`code` is one of `bad_request` (400), `not_found` (404), `conflict` (409), `unprocessable_entity` (422), `too_large` (413), `unavailable` (503) or `internal` (500). The message of a `500` is deliberately generic and the real error is logged.

Some older surfaces return a plain-text body instead of the envelope: the inbox message routes, the agents API, the conversation routes, the streams, analytics, and the inbound webhook. The status code is still meaningful.

### Bodies

Request and response bodies are JSON with camelCase fields, with these exceptions:

- **Messages and notifications** serialize with their Go field names (`ID`, `SubscriberID`, `Subject`, …), because those types carry no JSON tags. See [the message object](notifications-and-messages#message-object).
- Integration `credentials` use snake_case keys on the unified endpoint and camelCase on the legacy channel endpoints.

### Pagination

There is no single scheme. Each list says which it uses: `after`/`next` keyset cursors on subscribers, contexts, env vars and inbound domains, `cursor`/`next` on notifications, `before`/`next` on messages, `after`/`nextKey` on layouts, and cursors returned by the inbound archive and drafts. Cursors are opaque.

### Identifiers in paths

`GET`, `PATCH` and `DELETE /v1/subscribers/{id}` take the subscriber's **internal id** (the `id` in the subscriber object). Every other subscriber path (`/session`, `/credentials`, `/preferences`) takes **your `subscriberId`**.
