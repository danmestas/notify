---
title: Quickstart
weight: 2
---

This takes you from nothing to a delivered in-app notification on one machine. It uses the insecure development key, so do not point real data at it.

You need the `notify` and `dagnats` binaries ([Install](../install)).

## 1. Start the dagnats sidecar

```bash
dagnats serve
```

Leave it running. notify expects NATS on `nats://127.0.0.1:4222` and the worker bridge on `http://127.0.0.1:8080`, which are what `dagnats serve` binds by default.

## 2. Create a tenant

Everything in notify belongs to a **scope**: an organization and an environment. `bootstrap` creates both and mints the first API key.

```bash
notify bootstrap -dev -org acme -env development
```

```json
{
  "apiKey": "9f2c…",
  "orgId": "01J…",
  "envId": "01J…"
}
```

The key is shown once; notify stores only a hash. Keep it:

```bash
export KEY=9f2c…
```

`bootstrap` writes to the SQLite file named by `-db` (default `notify.db` in the current directory). Run it from the same directory, or pass the same `-db`, as `serve`. If you lose the key later, `notify rotate-key` mints a new one.

## 3. Start notify

```bash
notify serve -dev
```

`-dev` lets notify start without `NOTIFY_MASTER_KEY_HEX` by deriving a fixed, publicly known key. Outside development, generate a real one and set it on every command that opens the database:

```bash
export NOTIFY_MASTER_KEY_HEX=$(openssl rand -hex 32)
```

notify listens on `127.0.0.1:3000` (`-addr`). On start it checks that the dagnats bridge is reachable and that the bridge accepts its credentials, and it exits if not. Check it:

```bash
curl -s localhost:3000/healthz
# {"status":"ok","version":"0.0.0-dev"}
curl -s localhost:3000/readyz
# {"status":"ok","bridge":true,"db":true}
```

## 4. Create a workflow

A workflow is a trigger identifier and a list of steps. This one puts a message in the subscriber's in-app inbox.

```bash
curl -s -X POST localhost:3000/v1/workflows \
  -H "Authorization: ApiKey $KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Order shipped",
    "triggerIdentifier": "order-shipped",
    "steps": [{
      "stepId": "inbox",
      "type": "in_app",
      "controlValues": {
        "subject": "Hi {{subscriber.firstName}}",
        "body": "Order {{payload.orderId}} has shipped."
      }
    }]
  }'
```

The response is `201` with the workflow's `id`, `triggerIdentifier`, `name` and `active`. New workflows are active.

## 5. Trigger it

```bash
curl -s -X POST localhost:3000/v1/events/trigger \
  -H "Authorization: ApiKey $KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "order-shipped",
    "to": {"subscriberId": "user-1", "firstName": "Ada", "email": "ada@example.com"},
    "payload": {"orderId": "A-100"}
  }'
```

```json
{"acknowledged":true,"status":"processed","transactionId":"01J…"}
```

The `to` object creates or updates the subscriber, so you do not have to create subscribers first. `transactionId` identifies this send; supply your own to make the call idempotent.

## 6. Read the inbox

```bash
curl -s "localhost:3000/v1/messages?subscriberId=user-1" \
  -H "Authorization: ApiKey $KEY"
```

```json
{
  "data": [{
    "ID": "01J…", "SubscriberID": "user-1", "Channel": "in_app", "Status": "sent",
    "Subject": "Hi Ada", "Content": "Order A-100 has shipped.",
    "Seen": false, "Read": false, "Archived": false, "…": "…"
  }],
  "unreadCount": 1,
  "unseenCount": 1
}
```

Message objects use Go field names (`ID`, `Subject`, `Content`); see [Messages](../../api/notifications-and-messages#message-object) for the full list.

Delivery is asynchronous: the run executes on the dagnats engine, so the message appears a moment after the trigger returns. See [Activity](../../console#pages) in the console, or `GET /v1/notifications`, to follow a run.

## Where next

- Send through a real channel: [Channels](../../channels) shows how to create an email, SMS, push or chat integration.
- Add steps: [Workflows](../../workflows) lists every step type.
- Let a browser read the inbox without your API key: [Subscriber sessions](../../inbox-and-agents/subscriber-sessions).
- Run it for real: [Operations](../../operations).
