---
title: Channel steps
weight: 1
---

The channel steps are `in_app`, `email`, `push`, `chat` and `sms`. Each renders its templates, applies the [gates](../#gates) (condition, throttle, preferences), sends, and records a message. The `stream` step has its own [page](../stream).

Fields below go in the step's `controlValues`. Text fields are [templates](../../templating). Every field is optional at the API level, but a step with an empty `body` sends an empty body.

Email, push, chat and SMS steps also accept:

| Field | Meaning |
|---|---|
| `integration` | The `identifier` of a specific [integration](../../../channels) to send through. Empty means the channel's primary integration. |

## in_app

Writes a message to the subscriber's inbox. Nothing is sent externally; the message is readable at `GET /v1/messages` and is pushed to open [inbox streams](../../../inbox-and-agents/inbox#live-updates).

| Field | Meaning |
|---|---|
| `subject` | Title. |
| `body` | Message body. Values are inserted as sent: the inbox treats the text as text, so a client must not render it as HTML. |
| `avatar` | Avatar URL. |
| `redirect` | `{"url": "...", "target": "_self"}`. `url` is required when `redirect` is present; `target` is one of `_self`, `_blank`, `_parent`, `_top`. |
| `primaryAction`, `secondaryAction` | Call-to-action buttons: `{"label": "...", "redirect": {"url": "...", "target": "..."}}`. `redirect` is optional. |
| `data` | A free-form JSON object delivered with the message. |
| `layoutKey` | A [layout](../../templating#layouts) to wrap the body in. |

`avatar`, `redirect`, the actions and `data` are stored on the message under `extras`.

## email

Sends to the subscriber's `email` address.

| Field | Meaning |
|---|---|
| `subject` | Subject line. Always inserted as plain text. |
| `body` | The body. By default it is the plain-text part. |
| `editorType` | `"html"` sends `body` as an HTML body and escapes inserted values for HTML. Anything else (or absent) sends plain text. |
| `htmlBody` | An HTML alternative. When set, `body` is the plain-text part, `htmlBody` the HTML part, and the message goes out as `multipart/alternative`. |
| `layoutKey` | A [layout](../../templating#layouts) to wrap the body (or `htmlBody`) in. |
| `from` | Overrides the integration's From address. |
| `replyTo` | Sets Reply-To. |
| `reply` | `true` opts the message into **reply-to-notification**: notify sets Reply-To to a tokenized mailbox on one of your verified [inbound domains](../../../inbound-email), and a reply is delivered to your [outbound webhooks](../../../streams-and-webhooks#outbound-webhooks) as an `email.reply` event. If inbound is not available for the scope, the message is sent without it. If arming the reply fails, the send fails. Default `false`. |

## push

Sends through the active push integration. The target is the subscriber's address for that provider; see [push addressing](../../../channels/push).

| Field | Meaning |
|---|---|
| `subject` | Title. (`title` is accepted as an older spelling.) |
| `body` | Body. |
| `priority` | Integer 1 (min) to 5 (max); `0` or absent uses the provider default. ntfy. |
| `tags` | Array of strings. ntfy. |
| `markdown` | `true` to render the body as Markdown. ntfy. |
| `data_json` | A JSON object whose values are all strings, as a string, for example `"{\"screen\":\"inbox\"}"`. Forwarded as native data to FCM and Expo (deep links, badges). Anything else is rejected with `data must be a JSON object with string values only`. |

For Web Push, the notification is sent to every browser subscription the subscriber has registered.

## chat

Posts to a chat provider. The address comes from the subscriber's channel credentials; see [chat addressing](../../../channels/chat).

| Field | Meaning |
|---|---|
| `body` | Message text. Escaped for the destination: HTML for Telegram messages, mrkdwn escapes for Slack, plain for the rest. |
| `integration` | As above. |

A Telegram subscriber with no `chat_id` is a permanent failure for that step.

## sms

Sends `body` to the subscriber's `phone`.

| Field | Meaning |
|---|---|
| `body` | Message text, inserted as plain text. |

## Inline throttle

Any channel step can also carry a throttle in its `controlValues`:

```json
{ "body": "…", "throttle": { "limit": 5, "windowSec": 3600 } }
```

Over the limit, the step records a message with status `throttled` and sends nothing; the run continues. This differs from the standalone [`throttle` step](../throttle), which fails the whole run.

## What gets recorded

Each channel step writes one message: channel, provider id, integration id, rendered `Subject` and `Content`, a `Status` of `sent`, `error`, `skipped` or `throttled`, `ErrorText` or `SkipReason` where they apply, and the `Seen`, `Read` and `Archived` flags. Terminal statuses are also published as delivery events for analytics, the console's live activity view and outbound webhooks.
