---
title: A2A
weight: 9
---

notify speaks [A2A](https://a2a-protocol.org) (v0.3, JSON-RPC) for each agent, so another agent or a service can send it a message and read the reply. The address is the agent's opaque id (`agents.id`, the `id` in `GET /v1/agents`). It never names the model, harness or receiver behind it.

| Route | Purpose |
|---|---|
| `GET /a2a/{agentId}/.well-known/agent-card.json` | The agent card. |
| `POST /a2a/{agentId}` | JSON-RPC 2.0: `message/send` and `tasks/get`. |

## Get a key

Mint a [client key](../../api/a2a-client-keys) in the environment that owns the agent. A key only reaches agents in its own environment; an agent in another environment is `404`, the same as an unknown id. A [subscriber session token](../../inbox-and-agents/subscriber-sessions) works too. Send it as `Authorization: Bearer <token>`. Tenant API keys are `401`.

## The card

The card starts from the receiver's own card (a mesh agent's published card, or one derived for a managed agent) and keeps its name, description, skills and extensions. notify then sets `url` (this route), `preferredTransport: "JSONRPC"`, `capabilities.streaming: false` and a bearer `securitySchemes` entry. The URL is built on `NOTIFY_PUBLIC_BASE_URL`; when that is unset, on the scheme and host of the request.

```sh
curl -H "Authorization: Bearer $A2A_KEY" \
  https://notify.example.com/a2a/$AGENT_ID/.well-known/agent-card.json
```

## Send a message

```sh
curl -X POST https://notify.example.com/a2a/$AGENT_ID \
  -H "Authorization: Bearer $A2A_KEY" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{
        "kind":"message","messageId":"m-1","role":"user",
        "parts":[{"kind":"text","text":"Summarise today."}]}}}'
```

The result is a `Task`. If the agent replies within a few seconds the task is already `completed`, with the reply as an `answer` artifact (and as `status.message`). Otherwise it is `working`; poll with `tasks/get`:

```sh
curl -X POST https://notify.example.com/a2a/$AGENT_ID \
  -H "Authorization: Bearer $A2A_KEY" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tasks/get","params":{"id":"<task id>"}}'
```

A task is `working`, `completed`, or `failed` (the turn ended without a reply).

## How a message maps

- `contextId` is the conversation. It is private to the credential: two keys using the same `contextId` get separate conversations. Leave it out to start a new conversation; the task returns its `contextId`.
- `messageId` makes a retry safe. Sending the same `messageId` again returns the same task and starts no second turn. Reusing it with different content is `-32602`.
- Text parts become the message text. File parts with inline text bytes (`text/*` or `application/json`) become attachments under the usual caps. File parts by URI, binary files and data parts are `-32602`.
- A task id is the id of the message that started it. `tasks/get` only finds tasks sent with the same credential to the same agent; anything else is `-32001`.
- The conversation shows in notify's transcript like any other. A key bound to a subscriber speaks as that subscriber; an unbound key speaks as nobody.

## Errors and limits

`message/stream` and `tasks/resubscribe` are not available yet, and any other method is `-32004` (UnsupportedOperation). A malformed body is `-32700` or `-32600`. Bodies are capped at 1 MiB (`413`). `message/send` is limited to 60 a minute per credential and 300 a minute per agent (`429`, with `Retry-After`).
