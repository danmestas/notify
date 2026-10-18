---
title: Chat
weight: 3
---

Channel `chat`.

## Providers

| `providerId` | Credentials | Notes |
|---|---|---|
| `discord` | `webhook_url` (**secret**) | An incoming webhook. The webhook is both the secret and the destination. |
| `telegram` | `bot_token` (**secret**) | A bot. |
| `slack` | `bot_token` (**secret**) | A bot token, sent as a Bearer. |
| `whatsapp` | `access_token` (**secret**), `phone_number_id` | WhatsApp Business (Meta Cloud). |
| `mastodon` | `base_url`, `access_token`, `visibility` | `visibility` is `public`, `unlisted`, `private` or `direct`; empty means `public`. |
| `bluesky` | `pds_url`, `identifier`, `app_password` | `pds_url` empty means `https://bsky.social`. `identifier` is the handle or DID. Use an app password, never the account password. Bluesky posts are public broadcasts. |
| `chat-webhook` | `webhook_url`, `hmac_secret` (**secret**, optional) | The rendered message is POSTed as JSON. |

## Addressing

The subscriber's address for a provider is stored under `address`:

```bash
curl -X PUT localhost:3000/v1/subscribers/user-1/credentials \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"providerId": "telegram", "channel": "chat", "credentials": {"address": "123456789"}}'
```

| Provider | What `address` is |
|---|---|
| `telegram` | The chat id. Required: a subscriber with none is a permanent failure for the step. |
| `discord` | Optional override of the destination webhook. Without it the integration's webhook is used. |
| `slack`, `whatsapp`, `mastodon` | The recipient the provider addresses: a channel or user id, a phone number, an `@handle`. |
| `bluesky` | Not used. |

Telegram messages are sent with HTML parse mode, so inserted values are HTML-escaped; Slack messages are escaped for mrkdwn.

## Two-way chat

Telegram and Discord can also be **inbound**: notify long-polls Telegram and holds a Discord gateway connection, routes what people say to an agent, and sends the reply back. That is configured with environment variables, not integrations: see [Chat loop](../../inbox-and-agents/chat-loop). Mastodon and Bluesky mention polling reads its credentials from the matching integration instead.

## Legacy create

`POST /v1/integrations/chat` takes `provider` (`discord`, `telegram`, `slack`, `whatsapp`, `mastodon`, `bluesky`), `identifier`, `primary`, and `webhookUrl`, `botToken`, `accessToken`, `phoneNumberId`, `baseUrl`, `visibility`, `pdsUrl`, `accountIdentifier`, `appPassword` as needed.

## Step options

`body`, `integration`. See the [chat step](../../workflows/step-types/channel-steps#chat).
