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

## Objects

The JSON formats are pinned by fixtures in `internal/agents/wire/testdata`; organization and environment ids are never part of them.

**Agent**

```json
{ "id": "a1", "identifier": "support", "name": "Support", "description": "", "runtime": "managed",
  "active": true, "meshSubject": "", "bridgeUrl": "",
  "managed": { "apiKeySet": true, "model": "…", "provider": "openrouter", "systemPrompt": "…" },
  "welcomeMessage": "", "labels": { "owner": "lou" }, "logo": "", "createdAt": "…", "updatedAt": "…" }
```

**Link**: `{id, agentId, integrationId, connectedAt, config?}`.

**Conversation**: `{id, agentId, integrationId, platformThreadId, subscriberId, status, createdAt, lastActivityAt}`. `platformThreadId` is the handle you pass back as `conversationId`; `status` starts as `open`.

**Conversation summary** (the cross-agent list): `{conversationId, agentIdentifier, agentName, subscriberId, platformThreadId, status, lastMessageAt, lastActivityId}`.

**Activity**: `{id, conversationId, role, content: {parts: [{type, text}], action?: {label, value}, attachments?: [{name, mediaType, text}]}, runId?, createdAt}`.

**Draft list**: `{drafts: […], cursor, gap}`; see [Drafts](../../inbox-and-agents/drafts) for the draft fields.
