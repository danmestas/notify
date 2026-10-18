---
title: Stream
weight: 6
---

A `stream` step publishes an event to the environment's **durable feed**, a JetStream stream that clients read over SSE at `GET /v1/feed/stream`. It is for pushing events to your own consumers rather than to a person.

```json
{ "stepId": "emit", "type": "stream", "controlValues": { "topic": "orders.{{payload.region}}", "body": "{\"id\": \"{{payload.orderId}}\"}" } }
```

| Field | Meaning |
|---|---|
| `topic` | Required. Template for the routing key. After rendering it must be a valid feed topic: dot-separated, non-empty tokens of letters, digits, `_` and `-`, at most 128 bytes. No spaces, `*`, `>` or `$`. A topic that fails after rendering fails the step before any other gate runs. |
| `body` | Template for the event content. Inserted as plain text. |

The step applies the same condition, throttle and preference gates as other channel steps. A sent event is published to the feed with the topic and the message JSON (`Channel: "stream"`, `Subject` = the topic, `Content` = the body); it does not go to the inbox. The message is recorded, and delivery events fire, as for other channels.

Consumers read the feed with [`GET /v1/feed/stream`](../../../streams-and-webhooks#the-feed), and external producers can push into the same feed through [stream sources](../../../streams-and-webhooks#stream-sources).
