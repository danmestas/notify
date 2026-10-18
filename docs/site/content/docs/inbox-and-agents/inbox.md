---
title: Inbox
weight: 1
---

The inbox is the `in_app` messages of a subscriber. These four routes accept either an API key or a [subscriber session](../subscriber-sessions).

## List

```bash
curl "localhost:3000/v1/messages?subscriberId=user-1&limit=20" -H "Authorization: ApiKey $KEY"
```

| Query | Meaning |
|---|---|
| `subscriberId` | Whose inbox. Ignored for a subscriber session, which fixes it. |
| `channel` | Defaults to `in_app` when omitted, so the feed never widens to every channel's delivery records. |
| `transactionId` | Narrow to one transaction. |
| `limit` | Page size. Default 50, maximum 200; a non-positive or non-numeric value is a `400`. |
| `before` | A message id; return only older messages. |
| `archived` | `true`, `false` or `all`. Default excludes archived messages. |

```json
{ "data": [ { "ID": "…", "Subject": "…", "Content": "…", "Seen": false, "Read": false, "Archived": false, "extras": {} } ],
  "next": "01J…", "unreadCount": 3, "unseenCount": 3 }
```

`next` is the id to pass as the next page's `before`; it is absent on the last page. `unreadCount` and `unseenCount` never include archived messages, even when you are viewing the archived tab. Message fields are listed in the [API reference](../../api/notifications-and-messages#message-object); the in-app rich fields (avatar, actions, redirect, data) are in `extras`.

Two compatibility switches exist: `?format=array` (or an `Accept` header containing `application/vnd.notify.v1+json`) returns the older bare JSON array, which filters on an exact `channel` and does not default it. And `?integrationId=<id>` returns up to 100 messages for one integration across all subscribers, as a bare array, for an API key only (a session gets `403`).

## Count

`GET /v1/messages/count?subscriberId=&channel=&unread=true&unseen=true&archived=` returns `{"count": n}`. `unread=true` counts unread messages, `unseen=true` unseen ones, and both together narrow further. It defaults `channel` to `in_app` and excludes archived messages unless `archived` says otherwise. Use it for a badge.

## Mark

Single message: `POST /v1/messages/{id}/{action}` with `{"subscriberId": "user-1"}` and an action of `seen`, `read`, `unread`, `archive` or `unarchive`. `204` on success. There is no `unseen`: once seen, always seen. A message that does not exist, or belongs to another subscriber, is a `404`, so the response cannot be used to probe for other people's messages.

Bulk: `POST /v1/messages/mark` with:

```json
{ "subscriberId": "user-1", "ids": ["01J…", "01J…"], "as": "read" }
```

`as` is `seen`, `read`, `unread`, `archived` or `unarchived`. Give `ids`, or `"all": true` for every message. Ids belonging to a different subscriber are silently left out.

## Live updates

`GET /v1/inbox/stream?subscriberId=user-1` is a Server-Sent Events stream. Each new in-app message is delivered as a `data: <json>` frame; a `: ping` comment is sent every 15 seconds. The stream is best effort: a client too slow to keep up misses events, so refetch the list after reconnecting. This route takes an **API key** only, not a subscriber session, so it is for your backend or a server-side proxy.

## Errors

These routes return plain-text error bodies (`unauthorized`, `not found`, …), unlike most `/v1` routes, which return the [JSON error envelope](../../api#errors).
