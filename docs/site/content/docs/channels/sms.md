---
title: SMS
weight: 4
---

Channel `sms`. The recipient is the subscriber's phone, and only once the subscriber has verified it: they request a code by SMS and confirm it through their own [subscriber session](../../inbox-and-agents/subscriber-sessions#verify-a-phone). The `phone` you write with `POST` or `PATCH /v1/subscribers` is stored unverified, and writing a different number clears an earlier verification. `GET /v1/subscribers/{id}` reports `phoneVerified`. A subscriber with no verified phone has sms steps skipped (`skipReason: "no_verified_phone"`), not failed.

SMS is off by default for a subscriber with no SMS preference; a workflow turns it on with its step's `defaultEnabled`. See [preferences](../preferences#sms-default).

Upgrading from a release without verification stops existing SMS workflows until subscribers verify and you set `defaultEnabled`; see the [upgrade note](../../operations/releases#upgrade-note-verified-phones-for-sms-130).

Every send, the verification codes included, is recorded as an `sms` message on its integration, with the same `status`, `errorClass` and delivery health as push and email. A verification message never records the code.

## Credential check

Twilio integrations are covered by the credential check (on credential change, on `POST /v1/integrations/{id}/verify`, and daily): an authenticated `GET` of the account resource, which never sends a message. `2xx` passes; `401` or `403` is a rejected credential and sets the integration `failing`. SignalWire and SNS have no probe yet.

## Providers

| `providerId` | Credentials |
|---|---|
| `twilio` | `account_sid`, `auth_token` (**secret**), `from`, `help_text` |
| `signalwire` | `project_id`, `token` (**secret**), `space_url`, `from`, `signing_key` (**secret**), `help_text` |
| `sns` | `access_key_id`, `secret_access_key` (**secret**), `region` |

`from` is the sender number. Amazon SNS publishes straight to the recipient number and has no `from`. `help_text` is the reply to HELP (see [keywords](#keywords)). `signing_key` is the SignalWire project's Signing Key, which signs its callbacks; see [callbacks](#inbound-texts-and-delivery-status).

## Inbound texts and delivery status

Twilio and SignalWire integrations can send notify the texts people send to your number, and the delivery status of the texts notify sends. Each integration has two URLs, shown on its page in the console (Integrations, open the integration):

| URL | Where it goes |
|---|---|
| `{base}/v1/inbound/sms/{integrationId}` | Paste it into the number's "A message comes in" webhook, HTTP POST. |
| `{base}/v1/inbound/sms/{integrationId}/status` | Nothing to paste: notify sends it as the `StatusCallback` of every text it sends through the integration (step sends and HELP replies), so the provider reports each one's status there. |

`{base}` is `NOTIFY_PUBLIC_BASE_URL` plus the base path notify is mounted at. The routes take no API key: the provider's signature is the credential, and the integration in the URL names the environment. A request that does not verify gets `403`, as does an id that names no Twilio or SignalWire integration or a deactivated one, which answers nothing and sends no HELP reply. A body over 64 KiB gets `413`. Without `NOTIFY_PUBLIC_BASE_URL` every callback gets `503`, because the signature covers the public URL and notify never trusts the `Host` header to rebuild it; sends then ask for no status callback. The URL you paste must be exactly the one notify computes, scheme and any port included.

| Provider | Header | Key | Scheme |
|---|---|---|---|
| Twilio | `X-Twilio-Signature` | The integration's `auth_token` | base64 HMAC-SHA1 of the URL followed by each form parameter's name and value, names sorted. |
| SignalWire | `X-SignalWire-Signature` (or `X-Twilio-Signature`) | The integration's `signing_key` (Dashboard, API Credentials), not the API token | hex HMAC-SHA1 of the URL followed by the raw body; the Twilio-compatible form above, over the same key, is also accepted. |

A SignalWire integration with no `signing_key` refuses every callback; set it with `PUT /v1/integrations/{id}` (`credentials.signing_key`) or in the console.

### Inbound texts

Each text is stored with the environment's [inbound messages](../../api/inbound) (`channel: "sms"`, with `body`, `from`, `recipient`, `integrationId`, `subscriberId`, `phoneVerified`) and sent to your [webhook endpoints](../../streams-and-webhooks#outbound-webhooks) as an `sms.inbound` event. The sender (normalised to E.164) is matched to a subscriber in the integration's environment by phone, preferring one who verified it. A match on an unverified phone is still delivered, with `phoneVerified: false`. A sender no subscriber has is delivered with no `subscriberId`. A provider retry of the same `MessageSid` is stored and sent once.

### Keywords

A text whose whole body, trimmed and in any case, is a keyword also does this:

| Keywords | Effect | Event |
|---|---|---|
| `STOP`, `STOPALL`, `UNSUBSCRIBE`, `CANCEL`, `END`, `QUIT` | The number is opted out in the environment. | `sms.opt_out`, when it was not already |
| `START`, `YES`, `UNSTOP` | The number's opt-out is lifted. From a number that is not opted out, nothing happens: it is just a text. | `sms.opt_in`, when there was an opt-out |
| `HELP`, `INFO` | notify replies through the same integration with `help_text`, or a default naming the number, at most once per number per integration every 10 minutes. The reply goes to the sender even if their phone is not verified, since it answers them. It is recorded as an `sms` message. | none |

Keywords never change [preferences](../preferences). The opt-out record alone stops sending, so `STOP` then `START` leaves a subscriber exactly as before: their own `sms` preference, and each workflow's `defaultEnabled`, decide as they did. The text and its opt change are stored together: if either fails, notify answers `500`, keeps neither, and the provider's retry applies both. A provider's retry of a text already handled changes nothing again, so a late retry of an old `STOP` cannot undo a newer `START`.

`help_text` cannot itself be a keyword (`422`), so a HELP reply never reads as a command. Set it to an empty string (or `null`) in `PUT /v1/integrations/{id}`, or empty the field in the console, to go back to the default; leaving it out keeps it.

An opted-out number gets no sms step: it is skipped with `skipReason: "opted_out"`, checked before preferences. The opt-out belongs to the number, so it holds for every workflow, including `critical` ones, which bypass preferences but not an opt-out, and for every subscriber with that number, including one added later. No preference can override it. Only `START` lifts it.

Twilio's Advanced Opt-Out (and the carriers) may also handle STOP themselves and stop delivering to the number. The text still reaches notify, which records the opt-out as above, so notify's view stays right and the skipped step says why rather than failing at the provider.

### Delivery status

A status callback updates the `sms` message notify sent with that `MessageSid` (kept on the message as `ProviderMessageID`):

| Provider status | Message |
|---|---|
| `accepted`, `queued`, `sending`, `sent` | Stays `sent`. |
| `delivered` | `delivered`. |
| `undelivered`, `failed` | `error`, with `ErrorText` naming the status and `ErrorCode`, and an `ErrorClass` from the code: `recipient` for 30003-30006, 21211, 21610, 21612, 21614 (unreachable, blocked, unknown, landline or unsubscribed numbers); `rate_limited` for 30001; `auth` for 30002; `provider_down` for 30008 and 30500; `rejected_message` otherwise. |

`delivered` and `error` are final. A repeated or late callback (a `sent` after `delivered`, a second `failed`) changes nothing, so callbacks may arrive in any order. A move to `error` counts once against the integration's delivery health; `recipient` failures never do, as elsewhere. Each change is sent to your webhook endpoints as an `sms.status` event. A callback can beat the send's own message row: one for a provider id notify has not stored yet is kept for an hour and applied when that message is written (the message is then written with the final status, and delivery health counts it then). After an hour it is dropped, as is one for a message notify never sent.

## Legacy create

`POST /v1/integrations/sms` takes `provider` (`twilio`, `signalwire`, `sns`), `identifier`, `primary`, `from`, and `accountSid`, `authToken`; `projectId`, `token`, `spaceUrl`; or `accessKeyId`, `secretAccessKey`, `region`.

## Step options

`body`, `integration`, `defaultEnabled`. See the [sms step](../../workflows/step-types/channel-steps#sms).
