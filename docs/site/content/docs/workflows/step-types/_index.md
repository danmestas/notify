---
title: Step types
weight: 2
cascade:
  type: docs
---

notify has eleven step types. The list is the registry in `internal/steptype`; it is also what `POST /v1/workflows` accepts in `type`.

| Type | Kind | What it does |
|---|---|---|
| [`in_app`](channel-steps#in_app) | Channel | Puts a message in the subscriber's inbox. |
| [`email`](channel-steps#email) | Channel | Sends an email through an email integration. |
| [`push`](channel-steps#push) | Channel | Sends a push notification (ntfy, Expo, FCM, Web Push, or a push webhook). |
| [`chat`](channel-steps#chat) | Channel | Posts to Discord, Telegram, Slack, WhatsApp, Mastodon, Bluesky, or a chat webhook. |
| [`sms`](channel-steps#sms) | Channel | Sends an SMS through Twilio, SignalWire or Amazon SNS. |
| [`delay`](delay) | Flow | Waits: a fixed time, until a scheduled time of day, or until a time in the payload. |
| [`digest`](digest) | Flow | Collects events for a window and sends one run with the batch. |
| [`throttle`](throttle) | Flow | Fails the run when a subscriber exceeds a rate. |
| [`http_request`](http-request) | Action | Calls an HTTP endpoint and exposes the response to later steps. |
| [`stream`](stream) | Channel | Publishes an event to the environment's durable feed. |
| [`agent_message`](agent-message) | Action | Posts a message to an agent's conversation and starts its turn. |

All steps run in the order listed in the workflow. Channel steps accept `filters`, a [condition](../conditions).

## Gates

Channel steps (`in_app`, `email`, `push`, `chat`, `sms`, `stream`) apply their gates in this order:

1. The step's condition. False, or not evaluable, records a message with status `skipped` and `skipReason` `condition` or `condition_invalid`.
2. The step's inline `throttle` (`{"limit": n, "windowSec": s}` in `controlValues`), if set. Over the limit records a message with status `throttled`. The budget is per environment, subscriber and workflow.
3. The subscriber's [preferences](../../channels/preferences). A disabled channel records `skipped` with `skipReason` `preference`, unless the workflow is `critical`. If the preference lookup itself fails, the send proceeds.
4. The send. A transient provider error (a timeout or a 5xx) makes the engine retry the task. A permanent error records a message with status `error` and the error text.

Fields in `controlValues` that hold text are [Handlebars templates](../templating).
