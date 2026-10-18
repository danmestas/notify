---
title: Notifications and messages
weight: 4
---

## Notifications

A notification is one trigger for one recipient. All routes take an API key.

| Method and path | Purpose |
|---|---|
| `GET /v1/notifications` | List. Query: `workflowId`, `subscriberId`, `transactionId`, `since` (RFC 3339 lower bound on creation), `limit` (1 to 200, default 50), `cursor`. Returns `{"items": […], "next": "…", "totalCount": n}`; `next` is absent on the last page. |
| `GET /v1/notifications/{id}` | One notification; `404` if absent. |
| `GET /v1/notifications/{id}/messages` | The messages the notification produced, newest first. |

A notification serializes with Go field names: `ID`, `OrgID`, `EnvID`, `WorkflowID`, `SubscriberID`, `TransactionID`, `RunID`, `Payload`, `CreatedAt`, `DigestKey`.

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

Serialized with Go field names, plus a lowercase `extras`:

| Field | Meaning |
|---|---|
| `ID` | Message id (ULID). |
| `OrgID`, `EnvID` | Scope ids. |
| `WorkflowID` | Internal workflow id. |
| `WorkflowIdentifier` | The workflow's trigger identifier, when the workflow still exists. |
| `NotificationID`, `RunID`, `StepID` | The notification, engine run and step that produced it. |
| `SubscriberID` | Recipient. |
| `Channel` | `in_app`, `email`, `push`, `chat`, `sms` or `stream`. |
| `ProviderID`, `IntegrationID` | The provider and integration that sent it. |
| `Subject`, `Content` | The rendered subject and body. For `stream`, `Subject` is the topic. |
| `Status` | `sent`, `error`, `skipped` or `throttled`. |
| `ErrorText` | The failure, when `Status` is `error`. |
| `SkipReason` | `condition`, `condition_invalid` or `preference`, when `Status` is `skipped`. |
| `Seen`, `Read`, `Archived` | Inbox state. |
| `TransactionID` | The transaction. |
| `CreatedAt` | RFC 3339. |
| `extras` | For `in_app`: `avatar`, `primaryAction`, `secondaryAction`, `redirect`, `data`. `{}` otherwise. |

## Analytics

| Method and path | Purpose |
|---|---|
| `GET /v1/analytics/deliveries` | Delivery counts as `{channel: {status: count}}`. |
| `GET /v1/analytics/timeseries` | Array of `{day, channel, status, count}`. |

Neither takes parameters. The console has per-workflow and date views that the API does not expose.
