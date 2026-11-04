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

The card starts from the receiver's own card (a mesh agent's published card, or one derived for a managed agent) and keeps its name, description, skills and extensions. notify then sets `url` (this route), `preferredTransport: "JSONRPC"`, `capabilities.streaming: true` and a bearer `securitySchemes` entry. The URL is built on `NOTIFY_PUBLIC_BASE_URL`; when that is unset, on the scheme and host of the request.

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

A task is `working`, `completed`, `failed` (the turn ended without a reply), `input-required` or `canceled` (the agent chose that state). While it runs, `metadata.notes` lists the progress notes the agent reported ("Running search_vault: ..."), and `status.message` is the latest one. Once it ends, `metadata.usage` holds the tokens and cost the agent reported. The notes are stored, so `tasks/get` still returns them after a restart.

## Follow a task live

`message/stream` sends a message and answers as server-sent events (`Content-Type: text/event-stream`). Each event is a JSON-RPC response with your request id; its `result` is one of:

1. a `Task` (the first event; `status.state` is `submitted` until the agent reports progress);
2. a `TaskStatusUpdateEvent` (`kind: "status-update"`) with `state: "working"` and the note as `status.message`, one per note, in order;
3. a `TaskArtifactUpdateEvent` (`kind: "artifact-update"`) with the reply as artifact `answer` and `lastChunk: true`;
4. a closing `status-update` with `final: true`, the end state, the reply as `status.message` and any `metadata.usage`.

The server closes the stream after the closing event.

```sh
curl -N -X POST https://notify.example.com/a2a/$AGENT_ID \
  -H "Authorization: Bearer $A2A_KEY" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"message/stream","params":{"message":{
        "kind":"message","messageId":"m-2","role":"user",
        "parts":[{"kind":"text","text":"Find last week'"'"'s notes."}]}}}'
```

```
data: {"jsonrpc":"2.0","id":1,"result":{"kind":"task","id":"01J...","status":{"state":"submitted"}, ...}}

data: {"jsonrpc":"2.0","id":1,"result":{"kind":"status-update","taskId":"01J...","status":{"state":"working","message":{"parts":[{"kind":"text","text":"Running search_vault: last week"}], ...}},"final":false}}

data: {"jsonrpc":"2.0","id":1,"result":{"kind":"artifact-update","taskId":"01J...","artifact":{"artifactId":"...","name":"answer","parts":[...]},"lastChunk":true}}

data: {"jsonrpc":"2.0","id":1,"result":{"kind":"status-update","taskId":"01J...","status":{"state":"completed", ...},"final":true,"metadata":{"usage":{...}}}}
```

If your connection drops, `tasks/resubscribe` with `{"id":"<task id>"}` opens the same stream from the task's current state (a `Task` event first, with the notes so far in `metadata.notes`). A stream lasts at most 15 minutes; resubscribe to keep following.

Notes appear only when the agent reports progress. A mesh agent gets `metadata.stream: true` on the prompt and may send `status-update` and `artifact-update` frames ahead of its final message; an agent that sends the final message alone still works, and its task goes straight from `submitted` to `completed`.

### WebSocket

`GET /a2a/$AGENT_ID/ws` upgrades to a WebSocket (same `Authorization: Bearer` header). Send JSON-RPC requests as text frames: `message/send`, `message/stream`, `tasks/resubscribe` and `tasks/get`. Each response, and each stream event, arrives as a text frame carrying the JSON-RPC response with your request id, in the same shapes as above. Several requests can be in flight on one socket (up to 8); a stream ends with the `final: true` event.

```js
const ws = new WebSocket(url, { headers: { Authorization: `Bearer ${key}` } }); // e.g. the `ws` package
ws.on('open', () => ws.send(JSON.stringify({
  jsonrpc: '2.0', id: 1, method: 'message/stream',
  params: { message: { kind: 'message', messageId: 'm-3', role: 'user',
                       parts: [{ kind: 'text', text: 'Summarise today.' }] } },
})));
ws.on('message', (data) => {
  const { result } = JSON.parse(data);
  if (result.kind === 'status-update' && result.final) ws.close();
});
```

## How a message maps

- `contextId` is the conversation. It is private to the credential: two keys using the same `contextId` get separate conversations. Leave it out to start a new conversation; the task returns its `contextId`.
- `messageId` makes a retry safe. Sending the same `messageId` again returns the same task and starts no second turn. Reusing it with different content is `-32602`.
- Text parts become the message text. File parts with inline text bytes (`text/*` or `application/json`) become attachments under the usual caps. File parts by URI, binary files and data parts are `-32602`.
- A task id is the id of the message that started it. `tasks/get` only finds tasks sent with the same credential to the same agent; anything else is `-32001`.
- The conversation shows in notify's transcript like any other. A key bound to a subscriber speaks as that subscriber; an unbound key speaks as nobody.

## Errors and limits

Any method other than `message/send`, `message/stream`, `tasks/get` and `tasks/resubscribe` is `-32004` (UnsupportedOperation). A malformed body is `-32700` or `-32600`. Bodies are capped at 1 MiB (`413`). Every request (`message/send`, `message/stream`, `tasks/get`, `tasks/resubscribe`, a WebSocket upgrade and each WebSocket request) counts against 60 a minute per credential and 300 a minute per agent (`429`, with `Retry-After`; over a WebSocket, error `-32000`). A task is reachable only with the credential that sent it, to the same agent; streaming it with another credential is `-32001`.
