---
title: Concepts
weight: 2
---

The vocabulary used everywhere else in these docs.

## Scope: organization and environment

Every object belongs to one **organization** and one **environment** (for example `acme` and `production`). Together they form a **scope**. An API key is bound to exactly one scope, and every query it makes is filtered to that scope. Two environments never see each other's subscribers, workflows, integrations or messages, and a key from one cannot read the other.

`notify bootstrap -org <name> -env <name>` creates an organization, an environment and the first key. The console can add more environments (Settings, Environments).

## API keys and sessions

There are four credentials. They are not interchangeable.

| Credential | Header | Used for |
|---|---|---|
| API key | `Authorization: ApiKey <key>` | All `/v1` routes. A backend credential: keep it off devices. |
| Subscriber session | `Authorization: Bearer <token>` | Only the inbox message routes, and only for one subscriber. Minted by your backend. See [Subscriber sessions](../inbox-and-agents/subscriber-sessions). |
| Source key | `Authorization: SourceKey <key>` | Only `POST /v1/sources/{sourceID}/events`. See [Streams and webhooks](../streams-and-webhooks). |
| Inbound secret | `X-Inbound-Secret: <secret>` | Only `POST /v1/inbound/email`. See [Inbound email](../inbound-email). |

API keys are stored as a hash and shown once, at creation. `notify rotate-key` replaces the key for a scope and **revokes every existing key for it**.

## Subscribers

A **subscriber** is a recipient. You identify them with your own `subscriberId` string (a user id, for example), and optionally give an email address, phone number, first and last name, locale, timezone and a free-form `data` object. Templates read these as `{{subscriber.email}}`, `{{subscriber.firstName}}` and so on.

You can create subscribers explicitly with `POST /v1/subscribers`, or implicitly: the `to` object of a [trigger](../triggers) creates or updates its subscriber.

A subscriber also has **channel addresses** for providers that need one (an ntfy topic, a Telegram chat id, a browser's Web Push subscription) and **preferences** (which channels they accept, per workflow, and their digest cadence). See [Channels](../channels).

## Topics

A **topic** is a named group of subscribers with a `key`. Triggering a workflow `to.topicKey` fans out to every member, one run each, under one transaction id.

## Workflows

A **workflow** is a `triggerIdentifier` (a lowercase slug such as `order-shipped`) plus an ordered list of **steps**. Triggering the identifier runs the steps in order for a recipient. A workflow can be inactive (triggers are refused), and can be marked **critical**, which means subscribers cannot opt out of it. See [Workflows](../workflows).

## Integrations

An **integration** is a stored provider configuration for a channel: an SMTP account, a Resend API key, an ntfy server, a Telegram bot, a Twilio account, and so on. Credentials are encrypted at rest with the master key and never returned by the API (secret fields come back masked). A channel can have several integrations; the one marked **primary** is used unless a step names another by `identifier`. See [Channels](../channels).

## Runs, notifications and messages

Triggering a workflow produces three kinds of record:

- A **run** on the dagnats engine: the durable execution of the workflow's steps for one recipient. Runs survive restarts.
- A **notification**: one row per trigger per recipient, tying the transaction id, subscriber, workflow and run together. `GET /v1/notifications`.
- **Messages**: one row per delivery attempt that a channel step records, with channel, provider, status (`sent`, `error`, `skipped` or `throttled`), the rendered subject and content, and the seen, read and archived flags. The in-app inbox is the list of `in_app` messages. `GET /v1/messages`, `GET /v1/notifications/{id}/messages`.

A **transaction id** names one logical send. If you supply it on a trigger, repeating the call with the same id and recipient does not start a second run.

## Agents and conversations

An **agent** is a conversational endpoint registered in a scope. A **conversation** is a thread between an agent and a subscriber, with an append-only transcript. Messages posted to an agent start a turn that runs durably on the engine, using the agent's **runtime**: `mesh` (dispatched over NATS to an external agent) or `managed` (notify calls a model for you). See [Inbox and agents](../inbox-and-agents).

## Supporting objects

| Object | Purpose |
|---|---|
| Layouts | HTML wrappers for email bodies, with the rendered body injected as `{{content}}`. |
| Translations | Per-locale key/value maps; templates call `{{t "key"}}`. |
| Contexts | Typed records keyed by `type` and `id`, with JSON `data`, managed through `/v1/contexts`. |
| Environment variables | Per-environment key/value pairs, optionally secret (encrypted). Managed through `/v1/env-vars`. |
| Outbound webhooks | URLs that receive delivery-status events and email replies, signed with HMAC. |
| Inbound domains | Domains you receive reply mail on. |
| Stream sources | Credentialed producers that push events into the environment's feed. |

## How a trigger runs

1. The API authenticates the key and resolves the scope.
2. The trigger service finds the workflow by `name`, refuses if it is missing, inactive or empty, and upserts the recipient.
3. It starts a run on the dagnats engine, with an idempotency key built from the transaction id and the subscriber.
4. The engine dispatches each step as a task. notify's worker pool polls the sidecar's HTTP bridge, runs the step (render, check preferences, send, record a message) and reports back.
5. Regular delays and digests are engine-native sleeps, and so are scheduled and dynamic delays where the engine can express them (see [Delay](../workflows/step-types/delay)). Those hold no notify worker while waiting and a restart loses nothing. The remaining waits run as long-lived tasks that recompute their remaining time after a restart.
6. Delivery-status events go on a JetStream stream, which feeds analytics, the console's live activity view, and your outbound webhooks.
