---
title: Agents and conversations
weight: 3
---

An **agent** is registered in a scope under a unique `identifier`. A **conversation** is a thread between an agent and a subscriber. You post a message into a conversation; notify appends it to the transcript, starts the agent's turn as durable engine work, and lands the reply back as an agent entry in the transcript and a message in the subscriber's inbox, with a live event for open streams.

## Registering an agent

```bash
curl -X POST localhost:3000/v1/agents \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{
    "identifier": "support",
    "name": "Support",
    "description": "Answers support questions",
    "runtime": "mesh",
    "meshSubject": "agents.prompt.support",
    "welcomeMessage": "How can I help?",
    "labels": {"owner": "lou"}
  }'
```

| Field | Meaning |
|---|---|
| `identifier` | Required. Lowercase letters, digits, hyphens and underscores, starting with a letter or digit. Unique per environment (`409` on a duplicate). |
| `name`, `description`, `welcomeMessage` | Free text, returned as written. |
| `runtime` | `mesh` or `managed`. Anything else is `422`. |
| `active` | Default `true`. A workflow's `agent_message` step refuses an inactive agent. |
| `meshSubject` | For `mesh`: the NATS subject prompts are published to. |
| `managed` | For `managed`: the model settings. See [Managed agents](../managed-agents). |
| `labels` | Free-form tags. See below. |
| `logo` | Optional. A `data:` URI (`image/png`, `image/jpeg` or `image/webp`, base64), at most 96 KB decoded; anything else, SVG included, is `422`. `PATCH` with `""` clears it. |

`GET /v1/agents`, `GET /v1/agents/{identifier}`, `PATCH /v1/agents/{identifier}` (a partial update: only the fields you send change) and `DELETE /v1/agents/{identifier}` (`204`) complete the CRUD. `bridgeUrl` appears in responses and is accepted, but there is no `bridge` runtime: only `mesh` and `managed` can be created or switched to.

An agent object:

```json
{ "id": "…", "identifier": "support", "name": "Support", "description": "", "runtime": "mesh",
  "active": true, "meshSubject": "agents.prompt.support", "bridgeUrl": "",
  "welcomeMessage": "", "labels": {"owner": "lou"}, "logo": "", "createdAt": "…", "updatedAt": "…" }
```

`managed` is included for managed agents; organization and environment ids are never part of the format.

### Labels

Labels are string key/value tags that notify gives no meaning to, for example so a client can limit which agents a user may pick. At most 16 labels; keys are 1 to 63 characters of lowercase letters, digits, `.`, `_` and `-`; values are at most 128 characters and contain no control characters. `PATCH` with `labels` **replaces** the set; `{}` clears it. Filter a listing with `GET /v1/agents?label=owner:lou`; repeat `label` to require several, all of which must match. A `label` without a colon is a `400`.

## Runtimes

### mesh

notify publishes an A2A v2 prompt envelope (`{messageId, role: "user", parts: [{text}]}`) over NATS to the agent's `meshSubject`, with a reply-to inbox, and collects the answer, whether it arrives as one reply envelope or as streamed `role: "agent"` frames ending in a terminal task state. One dispatch is bounded at 90 seconds.

- Subjects must match the allowlist `NOTIFY_MESH_ALLOW` (`-mesh-allow`), default `agents.prompt.>`. Anything else is refused as a permanent failure.
- Dispatch uses `NOTIFY_MESH_NATS_URL` (`-mesh-nats-url`) if set, otherwise the engine's NATS URL. Set it when agents live on a different hub from the engine.
- An agent that acknowledges but never answers in v2 is reported as a permanent failure, as is a non-200 service error in the 4xx class.

### managed

notify itself calls a model. See [Managed agents](../managed-agents).

### Turn execution

Each turn is an engine task. A transient failure is retried: up to 3 retries after the first attempt, 2 seconds apart, then the turn goes to the dead-letter queue. A permanent failure goes there directly. Turns within a conversation are serialized, so they never interleave. A runtime receives the last 20 entries of the transcript with the new message.

## Composing a message

```bash
curl -X POST localhost:3000/v1/inbox/conversations/support/messages \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: msg-8841' \
  -d '{"subscriberId": "user-1", "conversationId": "", "text": "Where is my order?"}'
```

```json
{ "conversationId": "01J…", "activityId": "01J…", "runId": "…" }
```

It answers `202`: the turn runs as engine work, never inside the request.

| Field | Meaning |
|---|---|
| `subscriberId` | Required. |
| `text` | Required. |
| `conversationId` | The thread handle. Empty starts a new conversation; pass the one you were given to continue it. |
| `attachments` | Optional text files: `[{"name", "mediaType", "text"}]`, for example an email as Markdown. At most 8; `text` at most 64 KB each and 256 KB in all; `name` at most 200 characters. Stored with the message and returned by the transcript as `content.attachments`. A managed agent reads each one after your text as `Attached: <name> (<mediaType>)` and a fenced block, in later turns too. Over a limit is `422`. |

`Idempotency-Key` is optional: 1 to 64 characters of `A-Z`, `a-z`, `0-9`, `_`, `-` (`422` otherwise). Retrying a compose with the same key in the same conversation returns the original message, conversation and run instead of posting twice; a new conversation opened with a key gets a thread id derived from it, so a retry of the opening message lands in the same thread. Reusing a key for a *different* message is `409`.

Errors: `404` unknown agent, `422` for a missing field or a runtime that cannot execute on this server, `409` for a key conflict.

## Sources

A managed agent's reply lists what it drew on in `content.sources`: one entry per tool call it made, in order, then each attachment of the user's message it read, at most 20. An entry is `{"server", "tool", "label", "ref"?, "error"?}`. `server` and `tool` name the MCP server and tool; `label` is the call's `query`, `id`, `path` or `title` argument (the first present, cut to 120 characters), else the tool name; `ref` is the `id` or `path` argument when there is one; `error` is `true` for a call that failed. An attachment is `{"server": "", "tool": "attachment", "label": "<name>"}`. Mesh agents report no sources.

## Reading conversations

| Route | Returns |
|---|---|
| `GET /v1/inbox/conversations/{agentIdentifier}?conversationId=…` | `{"conversation": …, "activities": […]}`. `conversationId` is required (`400`). With no paging parameters you get the whole transcript, oldest first. With `limit`, `before` or `after` (activity ids) you get a page. When both `before` and `after` are given, `before` wins; with neither, the latest `limit` activities. |
| `GET /v1/agents/{identifier}/conversations?limit=&before=` | The agent's conversations, most recently active first. |
| `GET /v1/inbox/conversations?agent=&cursor=&limit=` | Conversations across all agents: `{"items": […], "next": "…"}`. `agent` filters by identifier. |

An activity: `{"id", "conversationId", "role", "content": {"parts": [{"type": "text", "text": "…"}], "action": {"label", "value"}?}, "runId"?, "createdAt"}`. `role` is `user`, `agent`, `system` or `action`. An agent may attach a suggested `action`; clicking it should be sent back as the next user message with `value` as the text.

## Channels

Agents can also be reached through chat integrations. `POST /v1/agents/{identifier}/integrations` with `{"integrationId": "…", "config": {…}}` links an integration (`201`; relinking updates `config`), `GET` lists links, and `DELETE /v1/agents/{identifier}/integrations/{integrationID}` removes one (`204`). `GET /v1/agents/{identifier}/channels/{integrationID}/setup` returns `{"webhookUrl": "{base_url}/v1/inbound/<integrationID>"}`. The `serve` command does not set a base URL on the agents service, so the response always carries the literal `{base_url}` placeholder for you to replace with your public origin.

## Email conversations

`POST /v1/agents/{identifier}/conversations/email` with `{"subscriberId", "text"}` starts a conversation and arms the [email reply loop](../../inbound-email/reply-loop) so the subscriber's reply goes back to the agent. It returns `201` with `{"conversationId", "replyTo"}`. It requires inbound email to be enabled, a verified inbound domain for the scope, and a subscriber with an email address; otherwise `422`.
