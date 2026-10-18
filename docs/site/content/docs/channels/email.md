---
title: Email
weight: 1
---

Channel `email`. The recipient is `subscriber.email`.

## Providers

`providerId` values for `POST /v1/integrations`, with the `credentials` they take. Fields marked **secret** are masked on read.

| `providerId` | Credentials |
|---|---|
| `smtp` | `addr` (`host:port`), `host`, `username`, `password` (**secret**), `from` |
| `sendmail` | `from` only. See below. |
| `resend` | `api_key` (**secret**), `from` |
| `ses` | `access_key_id`, `secret_access_key` (**secret**), `region`, `from` |
| `cloudflare-email` | `account_id`, `api_token` (**secret**), `from` |
| `email-webhook` | `webhook_url`, `hmac_secret` (**secret**, optional). The rendered email is POSTed to the URL as JSON, signed with the secret when set. |

`from` is the default sender. A step can override it with `from`.

### sendmail

The `sendmail` provider pipes each message to a local MTA command. The command is **host configuration, not tenant data**: it is `-sendmail-command` / `NOTIFY_SENDMAIL_COMMAND` (default `/usr/sbin/sendmail -ti`), split on whitespace and run without a shell. It is not an integration field because otherwise anyone with an API key could make notify run an arbitrary binary.

### Legacy create

`POST /v1/integrations/email` takes `provider` (`smtp` by default, `cloudflare-email`, `resend`, `ses`, `sendmail`), `from` (required), `identifier`, `primary`, plus `host`, `port`, `username`, `password` for SMTP, `accountId`, `apiToken` for Cloudflare, `apiKey` for Resend, and `accessKeyId`, `secretAccessKey`, `region` for SES.

## Step options

`subject`, `body`, `htmlBody`, `editorType`, `layoutKey`, `from`, `replyTo`, `reply`, `integration`. See the [email step](../../workflows/step-types/channel-steps#email).

## Replies

Setting `reply: true` on an email step makes the message carry a tokenized Reply-To at one of your [inbound domains](../../inbound-email), and delivers a reply to your [outbound webhooks](../../streams-and-webhooks#outbound-webhooks). Agent conversations started with `POST /v1/agents/{identifier}/conversations/email` use the same mechanism to put the subscriber's reply back in the conversation.
