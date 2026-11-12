---
title: Notifications and messages
weight: 4
---

## Notifications

A notification is one trigger for one recipient. All routes take an API key.

| Method and path | Purpose |
|---|---|
| `GET /v1/notifications` | List. Query: `workflowId`, `subscriberId`, `transactionId`, `status` (`pending`, `started`, `failed` or `cancelled`), `since` (RFC 3339 lower bound on creation), `limit` (1 to 200, default 50), `cursor`. Returns `{"items": […], "next": "…", "totalCount": n}`; `next` is absent on the last page. |
| `GET /v1/notifications/{id}` | One notification; `404` if absent. |
| `GET /v1/notifications/{id}/messages` | The messages the notification produced, newest first. |

A notification has the camelCase fields `id`, `workflowId`, `subscriberId`, `transactionId`, `runId`, `payload`, `createdAt`, `digestKey`, `status` and `runStartedAt` (when `runId` was recorded; empty while there is no run).

`status` is where the notification's run stands. A trigger records the notification before it starts the run, so the notification can exist before the run does:

| `status` | Meaning |
|---|---|
| `pending` | Recorded; the run has not been confirmed yet. `runId` is empty. A trigger whose start timed out leaves this, and retrying it with the same `transactionId` reuses the notification. One still pending with no run after 15 minutes is marked `failed`. |
| `started` | The run started; `runId` names it. Notifications recorded before this field existed read `started`. |
| `failed` | The engine refused the run, or the notification stayed `pending` for 15 minutes with no run. `runId` is empty. Retrying the trigger with the same `transactionId` tries again on the same notification, and a run that does turn up late records itself and moves the notification to `started`. |
| `cancelled` | The trigger was [cancelled](../../triggers#cancelling-a-trigger) before its run finished. `runId` names the run if one started. Nothing moves a notification out of `cancelled`. |

The list returns every status unless `status` is given.

### Status is not delivery

`status` tracks the run, not the messages. A notification stays `started` after its messages are sent, and it does not change when a message is delivered or fails. To check delivery, read the notification's messages at `GET /v1/notifications/{id}/messages`. Each message has its own `status`. To be told as delivery happens, subscribe to the delivery events in [Outbound webhooks](../../streams-and-webhooks/#outbound-webhooks).

## Messages

The inbox routes accept an API key or a [subscriber session](../../inbox-and-agents/subscriber-sessions). Behaviour is described under [Inbox](../../inbox-and-agents/inbox).

| Method and path | Purpose |
|---|---|
| `GET /v1/messages` | List: the paginated envelope `{data, next, unreadCount, unseenCount}`, or with `integrationId=` a bare array for one integration. |
| `GET /v1/messages/count` | `{"count": n}` for a badge. |
| `POST /v1/messages/mark` | Bulk mark `{subscriberId, ids \| all, as}`. `204`. |
| `POST /v1/messages/{id}/{action}` | `seen`, `read`, `unread`, `archive`, `unarchive`. `204`; `404` for a missing or someone else's message. |
| `GET /v1/inbox/stream?subscriberId=` | [Live SSE](../../inbox-and-agents/inbox#live-updates) of new in-app messages. API key only. |

### Message object

Every message endpoint (`GET /v1/messages` and `GET /v1/notifications/{id}/messages`) returns this camelCase shape:

| Field | Meaning |
|---|---|
| `id` | Message id (ULID). |
| `workflowId` | Internal workflow id. |
| `workflowIdentifier` | The workflow's trigger identifier, when the workflow still exists. |
| `notificationId`, `runId`, `stepId` | The notification, engine run and step that produced it. |
| `subscriberId` | Recipient. |
| `channel` | `in_app`, `email`, `push`, `chat`, `sms` or `stream`. |
| `providerId`, `integrationId` | The provider and integration that sent it. |
| `subject`, `content` | The rendered subject and body. For `stream`, `subject` is the topic. |
| `status` | `sent`, `error`, `skipped` or `throttled`. An `sms` message may later move from `sent` to `delivered` or `error` by [delivery status](../../channels/sms#delivery-status). |
| `providerMessageId` | The id the provider gave the message at send (an SMS `MessageSid`), when it gave one. |
| `errorText` | The failure, when `status` is `error`. |
| `errorClass` | Why it failed, when `status` is `error`: `auth` (the provider refused the credentials), `rate_limited`, `provider_down` (5xx, timeout, unreachable), `rejected_message` (the provider refused this message) `recipient` (a bad or unknown address) or `not_configured` (the scope has no integration for the channel). |
| `skipReason` | Why a step was skipped, when `status` is `skipped`: `condition`, `condition_invalid`, `preference`, `no_verified_phone` (sms), `opted_out` (sms: the number texted STOP), `no_email` (email), `no_device` (push) or `no_channel` (chat). A recipient with no address for the channel is skipped, not failed. |
| `seen`, `read`, `archived` | Inbox state. |
| `transactionId` | The transaction. |
| `createdAt` | RFC 3339. |
| `extras` | For `in_app`: `avatar`, `primaryAction`, `secondaryAction`, `redirect`, `data`. `{}` otherwise. |

Earlier releases serialized messages and notifications with Go field names (`ID`, `Status`, `ErrorText`, ...). That was a bug; the keys are now camelCase, and the scope ids (`OrgID`, `EnvID`) are no longer returned.

## Analytics

| Method and path | Purpose |
|---|---|
| `GET /v1/analytics/deliveries` | Delivery counts as `{channel: {status: count}}`. |
| `GET /v1/analytics/timeseries` | Array of `{day, channel, status, count}`. |

Neither takes parameters. The console has per-workflow and date views that the API does not expose.
