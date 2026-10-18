---
title: Chat loop
weight: 6
---

The chat loop lets people talk to an agent from Telegram or Discord. It dials **out** to those services, so it needs no public address.

- **Telegram**: notify long-polls the bot API with `TELEGRAM_BOT_TOKEN`.
- **Discord**: notify opens a gateway connection with `DISCORD_BOT_TOKEN`. In a server channel the bot answers only when it is @-mentioned, unless `NOTIFY_DISCORD_RESPOND_ALL` is `1` or `true`, which makes it answer every message (for a dedicated channel). Direct messages always route.
- **Mastodon and Bluesky**: mention polling reads its credentials from the environment's `mastodon` and `bluesky` [chat integrations](../../channels/chat), not from environment variables.

A bot token that is empty leaves that platform's poller off.

## Routing

A message from a chat that already has a conversation goes back to its agent. A **cold** message, from a chat with no binding, goes to the default agent:

| Variable | Meaning |
|---|---|
| `NOTIFY_CHAT_DEFAULT_AGENT` | Identifier of the agent that receives cold messages. Empty drops cold messages. |
| `NOTIFY_CHAT_DEFAULT_ORG`, `NOTIFY_CHAT_DEFAULT_ENV` | The scope (organization and environment ids) the default agent is looked up in, and in which the Mastodon and Bluesky integrations are read. |

The agent's reply is sent back into the chat it came from. The console's **Chat** page shows the loop's status.
