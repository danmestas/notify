---
title: Push
weight: 2
---

Channel `push`.

## Providers

| `providerId` | Credentials | Notes |
|---|---|---|
| `ntfy` | `base_url`, `token` (**secret**) | An ntfy server. `base_url` is required. |
| `expo` | `access_token` (**secret**, optional) | Expo push. Expo accepts unauthenticated sends for projects without an enforced access token. |
| `fcm` | `service_account` (**secret**, the service-account JSON), `project_id` | Firebase Cloud Messaging HTTP v1. Both are required. |
| `webpush` | none | Browser Web Push. notify mints the VAPID keypair itself; see below. |
| `push-webhook` | `webhook_url`, `hmac_secret` (**secret**, optional) | The rendered push is POSTed as JSON. |

## Addressing

For `ntfy`, `expo` and `fcm`, the destination comes from the subscriber's channel credentials, read from the `topic` field:

```bash
curl -X PUT localhost:3000/v1/subscribers/user-1/credentials \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"providerId": "ntfy", "channel": "push", "credentials": {"topic": "alerts-user-1"}}'
```

For `expo` the `topic` value is the device's `ExponentPushToken[…]`, and for `fcm` it is the device registration token. A subscriber with no topic for the active provider is a permanent failure for that step.

## Web Push

1. `GET /v1/integrations/webpush/key` returns `{"publicKey", "subject", "subjectDefault"}`, minting the environment's VAPID keypair on first use. Give the public key to your service worker's `pushManager.subscribe`.
2. Register each browser with `PUT /v1/subscribers/{subscriberId}/credentials` using `"providerId": "webpush"` and `credentials` set to the browser's `PushSubscription` JSON. `endpoint`, `keys.p256dh` and `keys.auth` are required. A subscriber can hold many devices; registering the same `endpoint` again updates it.
3. List a subscriber's devices with `GET /v1/subscribers/{subscriberId}/credentials/webpush`. Each device is `{"id", "endpointHash", "createdAt", "lastSuccessAt", "lastErrorClass"}`. `endpointHash` is the lowercase hex SHA-256 of the exact endpoint string, so a client finds "this browser" by hashing its own endpoint; the endpoint and keys are never returned. `lastSuccessAt` is empty until a push reaches the device, and `lastErrorClass` is the class of its last failed push, empty when the last one was delivered. A subscriber with no devices, or none at all, gets `{"data": []}`.
4. Remove one device with `DELETE /v1/subscribers/{subscriberId}/credentials/webpush/{id}`, where `{id}` is the device `id` or its `endpointHash`, or with `DELETE /v1/subscribers/{subscriberId}/credentials/webpush?endpoint=<urlencoded endpoint>`. Both answer `204`, or `404` when no such device exists.
5. Set the VAPID contact with `PUT /v1/integrations/webpush/subject` and `{"subject": "mailto:ops@acme.test"}`. Apple's push service requires a reachable contact; an unusable subject is a `400`. An empty subject clears your override, and the server default (derived from `NOTIFY_PUBLIC_BASE_URL`) applies. With neither, Web Push cannot send.

`subjectDefault` is a boolean, not a second address: `true` when `subject` is the server default, `false` when it is your override.

```json
{ "publicKey": "BNc…", "subject": "mailto:ops@acme.test", "subjectDefault": true }
```

When a push service reports a subscription dead (`404` or `410`), or refuses it three times in a row, notify deletes it and sends a [`webpush.subscription_removed`](../streams-and-webhooks#outbound-webhooks) event to your webhook endpoints. Deleting a device through either `DELETE` route sends the same event with reason `deleted`.

A push step sends to every browser subscription the subscriber has. The keypair lives in an integration row with provider `webpush`; delete that row to rotate the keys. If you already use another push provider, the existing primary keeps resolving first; make `webpush` primary, or call `PATCH /v1/integrations/{id}/active`, to switch.

## Legacy create

`POST /v1/integrations/push` takes `provider` (`ntfy` by default, `expo`, `fcm`), `identifier`, `primary`, and `baseUrl`, `token`, `accessToken`, `serviceAccount`, `projectId` as the provider needs.

## Step options

`subject`, `body`, `priority`, `tags`, `markdown`, `data_json`, `integration`. See the [push step](../../workflows/step-types/channel-steps#push).
