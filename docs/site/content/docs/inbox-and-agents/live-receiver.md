---
title: Live receiver
weight: 7
---

A live receiver lets you point your own harness at a notify agent and answer its turns in real time, in place of the agent's configured runtime. While a harness is connected, every turn of that agent goes to it, whichever channel the message came in on (the inbox, Bluesky, email, A2A, and so on). When it disconnects, turns go back to the configured runtime.

## Mint a receiver key

A receiver key belongs to one agent and can answer that agent's turns and nothing else. It is a different credential from an [A2A client key](../../api/a2a-client-keys), which calls notify; do not use one for the other. The secret looks like `a2arx_<64 hex>`, is shown once at mint, and is stored only as a hash.

| Method and path | Auth | Purpose |
|---|---|---|
| `POST /v1/agents/{identifier}/receiver-keys` | API key | Mint `{"name"}`. `201` with the key and its `secret`, shown once. |
| `GET /v1/agents/{identifier}/receiver-keys` | API key | List the agent's keys, revoked ones included: `{data: […]}`. |
| `DELETE /v1/agents/{identifier}/receiver-keys/{id}` | API key | Revoke. `204`; `404` when unknown or already revoked. Disconnects a receiver using it. |

You can also mint, list and revoke keys on the console's agent page (Overview tab).

## Connect

Open a WebSocket to `/a2a/{agentId}/receive`, where `{agentId}` is the agent's opaque `id` (the `id` field of `GET /v1/agents/{identifier}`, not its `identifier`). Send the key as `Authorization: Bearer a2arx_…`. A missing, unknown, revoked or other agent's key is `401`. The key is read from the header only, never the query string, because query strings end up in access logs.

One receiver is live per agent. A second connection replaces the first; the first is closed with the reason `replaced`.

notify pings every 20 seconds. A socket that does not answer is treated as disconnected. Any WebSocket client answers pings while it is reading.

## Frames

All frames are JSON text.

notify to harness, one per turn:

```json
{"type":"turn","message":{
  "messageId":"01J…","contextId":"01J…","role":"user",
  "parts":[{"text":"Hello"}],
  "metadata":{"history":"inline","subscriberId":"alice"}}}
```

`message` is the same strict A2A message the mesh would receive. `messageId` is stable for the turn: it is the id of the user's message, and it is the same if the turn is retried. The conversation so far is in the prompt text (`history` is always `inline` on this socket).

Harness to notify, for each turn it answers. `replyTo` is the `messageId` of the turn the frame belongs to; several turns can be in flight on one socket, answered in any order.

```json
{"kind":"status-update","replyTo":"01J…","status":{"state":"working"}}
{"kind":"message","role":"agent","replyTo":"01J…","contextId":"01J…","parts":[{"text":"Hi Alice."}]}
```

- `status-update` and `artifact-update` frames are accepted and currently ignored.
- The final `message` frame ends the turn; its text parts are joined and delivered as the agent's reply.
- `{"kind":"error","replyTo":"01J…","message":"why"}` fails the turn without retrying.

A turn the harness does not answer within 90 seconds fails and is retried.

## Takeover and fallback

If the socket drops while a turn is in flight, notify sends that same turn (same `messageId`) to the configured runtime, so an agent that deduplicates on `messageId` sees it once. Turns that arrive while no receiver is connected go straight to the runtime.

`GET /v1/agents` and `GET /v1/agents/{identifier}` report who answers: `"receiver": {"kind": "live", "since": "…"}` while a harness is connected, otherwise `{"kind": "mesh"}` (or the agent's runtime). The console agent page shows the same as "Answered by".

## Minimal client

```js
// node 22+, npm i ws
import WebSocket from "ws";

const ws = new WebSocket(`wss://notify.example.com/a2a/${AGENT_ID}/receive`, {
  headers: { Authorization: `Bearer ${RECEIVER_KEY}` },
});

ws.on("message", (raw) => {
  const { type, message } = JSON.parse(raw);
  if (type !== "turn") return;
  const text = message.parts.map((p) => p.text).join("");
  ws.send(JSON.stringify({
    kind: "message", role: "agent",
    replyTo: message.messageId, contextId: message.contextId,
    parts: [{ text: `You said: ${text}` }],
  }));
});
```
