---
title: Subscribers and topics
weight: 2
---

All routes take an API key.

## Subscribers

| Method and path | Purpose | Success |
|---|---|---|
| `POST /v1/subscribers` | Upsert by `subscriberId`. | `201` the subscriber |
| `GET /v1/subscribers` | List. `?after=<cursor>&limit=<n>` (`limit` must be a positive integer). | `200` `{data, next?, totalCount}` |
| `GET /v1/subscribers/{id}` | One subscriber, by **internal id**. | `200`; `404` |
| `PATCH /v1/subscribers/{id}` | Update by internal id. Only fields present and non-empty are applied. | `200` |
| `DELETE /v1/subscribers/{id}` | Soft delete, by internal id. | `204`; `404` |
| `POST /v1/subscribers/{subscriberId}/session` | Mint a [session token](../../inbox-and-agents/subscriber-sessions). | `200` `{token, expiresAt}`; `404` if the subscriber does not exist |
| `PUT /v1/subscribers/{subscriberId}/credentials` | Set the subscriber's address for a provider. | `204` |
| `DELETE /v1/subscribers/{subscriberId}/credentials/webpush?endpoint=` | Remove one Web Push device. | `204`; `400` without `endpoint`; `404` |
| `GET /v1/subscribers/{id}/preferences` | [Preferences](../../channels/preferences). `{id}` is your `subscriberId`. | `200` |
| `PUT /v1/subscribers/{id}/preferences` | Partially update them. | `200`; `404`; `422` |

The subscriber object:

```json
{ "id": "01J…", "subscriberId": "user-1", "email": "", "phone": "", "firstName": "", "lastName": "",
  "locale": "", "timezone": "", "data": { } }
```

`POST` and `PATCH` bodies take the same fields without `id`. Validation (a malformed email, an oversized field, too many or invalid custom `data` fields) is `400` with the reason.

`PUT .../credentials` takes `{"providerId": "ntfy", "channel": "push", "credentials": { … }}`; `providerId` is required. With `providerId: "webpush"`, `credentials` is a browser `PushSubscription` (`endpoint`, `keys.p256dh`, `keys.auth` required). See [push](../../channels/push) and [chat](../../channels/chat) for the fields.

## Topics

| Method and path | Purpose | Success |
|---|---|---|
| `POST /v1/topics` | Create or upsert `{"key", "name"}`. | `201` the topic; `422` for an invalid key |
| `GET /v1/topics` | List. | `200` `{data: […]}` |
| `GET /v1/topics/{key}` | One topic. | `200` `{id, key, name, memberCount}`; `404` |
| `PUT /v1/topics/{key}` | Rename: body `{"name"}` (required). Creates the topic if missing. | `200` |
| `DELETE /v1/topics/{key}` | Delete the topic and its memberships. | `204`; `404` |
| `POST /v1/topics/{key}/subscribers` | Add `{"subscriberId"}`. The subscriber must exist. | `204`; `400` empty; `404` unknown topic; `422` unknown subscriber |
| `DELETE /v1/topics/{key}/subscribers/{subscriberId}` | Remove a member. Removing a non-member is not an error. | `204`; `404` unknown topic |
