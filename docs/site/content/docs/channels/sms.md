---
title: SMS
weight: 4
---

Channel `sms`. The recipient is `subscriber.phone`.

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

`body`, `integration`. See the [sms step](../../workflows/step-types/channel-steps#sms).
