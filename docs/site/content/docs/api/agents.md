---
title: Agents and conversations
weight: 5
---

All routes take an API key. Concepts, runtimes and examples are in [Agents and conversations](../../inbox-and-agents/agents).

| Method and path | Purpose | Success |
|---|---|---|
| `POST /v1/agents` | Register an agent. | `201`; `409` duplicate; `422` invalid |
| `GET /v1/agents` | List; `?label=key:value` (repeatable) filters. | `200` array |
| `GET /v1/agents/{identifier}` | One agent. | `200`; `404` |
| `PATCH /v1/agents/{identifier}` | Partial update. `labels` replaces the set. | `200` |
| `DELETE /v1/agents/{identifier}` | Delete. | `204` |
| `PATCH /v1/agents/{identifier}/config` | Change a mesh agent's runner config. See below. Optional `Idempotency-Key`. | `200` `{card}`; `400`, `403`, `404`, `409`, `503` |
| `POST /v1/agents/{identifier}/integrations` | Link a channel integration: `{integrationId, config}`. | `201` the link |
| `GET /v1/agents/{identifier}/integrations` | List links. | `200` array |
| `DELETE /v1/agents/{identifier}/integrations/{integrationID}` | Unlink. | `204`; `404` |
| `GET /v1/agents/{identifier}/conversations` | The agent's conversations. `?limit=&before=` | `200` array |
| `GET /v1/agents/{identifier}/channels/{integrationID}/setup` | The inbound webhook URL for a linked channel. | `200` `{webhookUrl}`; `404` if not linked |
| `POST /v1/agents/{identifier}/conversations/email` | Start an email-origin conversation. `{subscriberId, text}` | `201` `{conversationId, replyTo}` |
| `POST /v1/inbox/conversations/{agentIdentifier}/messages` | Compose a message. Optional `Idempotency-Key`. | `202` `{conversationId, activityId, runId}` |
| `GET /v1/inbox/conversations/{agentIdentifier}` | Transcript. `conversationId` required; optional `limit`, `before`, `after`. | `200` `{conversation, activities}` |
| `GET /v1/inbox/conversations` | Conversations across agents. `?agent=&cursor=&limit=` | `200` `{items, next}` |
| `GET /v1/inbox/drafts` | [Drafts](../../inbox-and-agents/drafts). `?subscriberId=&mailbox=…&after=&limit=` | `200` `{drafts, cursor, gap}` |

The email conversation route exists only when the agent, inbound email and audience services are all present. These routes return plain-text error bodies, not the JSON envelope.

## Agent config

`PATCH /v1/agents/{identifier}/config` changes the runner configuration of a `mesh` agent whose `meshSubject` is under `agents.prompt.`. notify relays the JSON object body unchanged, up to 64 KiB, as a request to `agents.config.<tail>` (the prompt subject with `agents.prompt.` replaced), and waits up to 10 seconds. The body is `{model?, llmProvider?, instructions?, tools?, providerKey?}`; the agent validates it. `providerKey` is write-only: it appears in no response and notify never logs the body, only the caller's organization and environment, the agent and the outcome.

| Result | Status |
|---|---|
| The agent applied it. Body `{"card": …}`, the agent's updated card. The stored card follows through the card watch. | `200` |
| The agent refused it as `invalid`; the body is its message. Also a body that is not a JSON object, or over 64 KiB. | `400` |
| The agent answered `forbidden`. | `403` |
| Unknown agent, or one outside your organization and environment. | `404` |
| The agent is not runtime `mesh`, or its `meshSubject` is not under `agents.prompt.`. | `409` |
| Agent `unavailable`, not listening, or silent for 10 seconds. | `503` |

A malformed `Idempotency-Key` is a `422`. A config change sets fields rather than appending to them, so repeating a request with the same key leaves the agent in the same state.

## Objects

The JSON formats are pinned by fixtures in `internal/agents/wire/testdata`; organization and environment ids are never part of them.

**Agent**

```json
{ "id": "a1", "identifier": "support", "name": "Support", "description": "", "runtime": "managed",
  "active": true, "meshSubject": "", "bridgeUrl": "",
  "managed": { "apiKeySet": true, "model": "…", "provider": "openrouter", "systemPrompt": "…" },
  "welcomeMessage": "", "labels": { "owner": "lou" }, "logo": "", "history": "inline", "receiver": { "kind": "mesh" }, "card": null, "createdAt": "…", "updatedAt": "…" }
```

`receiver` says what answers the agent's turns: `{"kind": "live", "since": "…"}` while a harness is connected over the [live receiver](../../inbox-and-agents/live-receiver) WebSocket (on `GET /v1/agents` and `GET /v1/agents/{identifier}`), otherwise `{"kind": <runtime>}`.

`card` is the agent's A2A v0.3 AgentCard, returned by `GET /v1/agents` and `GET /v1/agents/{identifier}` only (create and patch answer `null`). A `mesh` agent publishes its own card; notify passes it through as JSON, with `cardUpdatedAt` (RFC 3339) saying when it was last written, and `card` is `null` until the agent publishes one or when its `meshSubject` is not under `agents.prompt.`. A `managed` agent with no stored card gets one derived from its settings on each request: name, description, `protocolVersion` `0.3.0`, and a runtime extension carrying `model`, `llmProvider` and `tools` (its MCP servers, kind `mcp`); it never includes the API key or any secret, and has no `cardUpdatedAt`. A card is visible only to callers who can see the agent, so an agent outside your organization and environment is a `404`.
**Link**: `{id, agentId, integrationId, connectedAt, config?}`.

**Conversation**: `{id, agentId, integrationId, platformThreadId, subscriberId, status, createdAt, lastActivityAt}`. `platformThreadId` is the handle you pass back as `conversationId`; `status` starts as `open`.

**Conversation summary** (the cross-agent list): `{conversationId, agentIdentifier, agentName, subscriberId, platformThreadId, status, lastMessageAt, lastActivityId}`.

**Activity**: `{id, conversationId, role, content: {parts: [{type, text}], action?: {label, value}, attachments?: [{name, mediaType, text}], sources?: [{server, tool, label, ref?, error?}]}, runId?, createdAt}`.

**Draft list**: `{drafts: […], cursor, gap}`; see [Drafts](../../inbox-and-agents/drafts) for the draft fields.
