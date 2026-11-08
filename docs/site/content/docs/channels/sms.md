---
title: SMS
weight: 4
---

Channel `sms`. The recipient is the subscriber's phone, and only once the subscriber has verified it: they request a code by SMS and confirm it through their own [subscriber session](../../inbox-and-agents/subscriber-sessions#verify-a-phone). The `phone` you write with `POST` or `PATCH /v1/subscribers` is stored unverified, and writing a different number clears an earlier verification. `GET /v1/subscribers/{id}` reports `phoneVerified`. A subscriber with no verified phone has sms steps skipped (`skipReason: "no_verified_phone"`), not failed.

SMS is off by default for a subscriber with no SMS preference; a workflow turns it on with its step's `defaultEnabled`. See [preferences](../preferences#sms-default).

Every send, the verification codes included, is recorded as an `sms` message on its integration, with the same `status`, `errorClass` and delivery health as push and email. A verification message never records the code.

## Credential check

Twilio integrations are covered by the credential check (on credential change, on `POST /v1/integrations/{id}/verify`, and daily): an authenticated `GET` of the account resource, which never sends a message. `2xx` passes; `401` or `403` is a rejected credential and sets the integration `failing`. SignalWire and SNS have no probe yet.

## Providers

| `providerId` | Credentials |
|---|---|
| `twilio` | `account_sid`, `auth_token` (**secret**), `from` |
| `signalwire` | `project_id`, `token` (**secret**), `space_url`, `from` |
| `sns` | `access_key_id`, `secret_access_key` (**secret**), `region` |

`from` is the sender number. Amazon SNS publishes straight to the recipient number and has no `from`.

## Legacy create

`POST /v1/integrations/sms` takes `provider` (`twilio`, `signalwire`, `sns`), `identifier`, `primary`, `from`, and `accountSid`, `authToken`; `projectId`, `token`, `spaceUrl`; or `accessKeyId`, `secretAccessKey`, `region`.

## Step options

`body`, `integration`, `defaultEnabled`. See the [sms step](../../workflows/step-types/channel-steps#sms).
