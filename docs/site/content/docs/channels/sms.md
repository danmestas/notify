---
title: SMS
weight: 4
---

Channel `sms`. The recipient is the subscriber's phone, and only once it counts as consented, in one of two ways:

- **Self-verified.** The subscriber requests a code by SMS and confirms it through their own [subscriber session](../../inbox-and-agents/subscriber-sessions#verify-a-phone).
- **Operator-attested.** Where the environment allows it, you state through the API that the subscriber agreed to texts under your privacy policy. See [operator-attested consent](#operator-attested-consent).

The `phone` you write with `POST` or `PATCH /v1/subscribers` is stored without consent, and writing a different number clears consent of either kind. `GET /v1/subscribers/{id}` reports `phoneVerified` and `phoneConsent` (`self_verified`, `operator_attested`, or absent). A subscriber with no consented phone has sms steps skipped (`skipReason: "no_verified_phone"`), not failed. A number that texted STOP is skipped whatever its consent; see [keywords](#keywords).

SMS is off by default for a subscriber with no SMS preference; a workflow turns it on with its step's `defaultEnabled`. See [preferences](../preferences#sms-default).

Upgrading from a release without verification stops existing SMS workflows until subscribers verify and you set `defaultEnabled`; see the [upgrade note](../../operations/releases#upgrade-note-verified-phones-for-sms-130).

Every send, the verification codes included, is recorded as an `sms` message on its integration, with the same `status`, `errorClass` and delivery health as push and email. A verification message never records the code.

## Operator-attested consent

A business whose customers agreed to texts through their dealings with it (in a shop, on a signed form), under its published privacy policy, can attest that consent instead of asking each customer to verify a code.

**You are responsible for having that consent.** notify does not check it. Attesting says you hold consent that meets the law that applies to you (in the US the TCPA, and carrier rules) and your own privacy policy, and notify records that you said so. A STOP from the number always wins over an attestation.

In the console it is one switch. Open Integrations and find the card *Text customers who opted in at your shop*: give the link to your privacy policy (it must start with `https://`), turn the switch on and Save. Turning it on does both things the API needs, in one step: it allows operator-attested consent for the environment, with that policy link, and lets the environment's key attest. Turning it off clears both; existing attestations are suspended (sms steps skip them), not deleted, and turning it back on restores them. A link that is missing or not `https` is refused and nothing changes.

**Whoever can log in to the console decides this for the shop.** The switch is not in the HTTP API on purpose: a key must not be able to turn it on for itself. The permission stays with the environment's key when you rotate it, in the console or with `notify rotate-key`; `notify rotate-key -attest-sms-consent=true|false` sets it on the new key.

For a headless setup with no console, an operator with access to the database sets the same two things separately with [`notify sms-consent`](../../configuration#commands), run on the host like `notify bootstrap`:

```sh
notify sms-consent -org Acme -env production -enable -policy-url https://shop.example/privacy   # the setting
notify sms-consent -org Acme -env production -grant-key 9f86d081   # the key's permission; the key's id, never the key
notify sms-consent -org Acme -env production -revoke-key 9f86d081
notify sms-consent -org Acme -env production -disable
```

Run with no action, it prints the setting and each key's id and permission. Granting or revoking changes only the permission, so the key keeps working as before. Each change is logged to stderr with the organization, environment and, for a key, its id.

| Method and path | Purpose | Success |
|---|---|---|
| `POST /v1/subscribers/{subscriberId}/phone/consent` | Attest consent for `{"phone", "source"?, "attestedAt"?}`. | `201` the attestation |
| `DELETE /v1/subscribers/{subscriberId}/phone/consent` | Revoke an attestation, with an optional `{"reason"}` (up to 500 bytes). Any API key of the environment may. | `204` |

`{subscriberId}` is your id for the subscriber, who must already exist in the key's environment (`404` otherwise, as for another environment's subscriber). `phone` is normalised to E.164 (`422` when it cannot be). If the subscriber's stored phone is a different number that nobody verified, it is replaced by this one. If the subscriber verified a different number themselves, the attestation is refused with `409` `phone_self_verified` and nothing changes: an attestation never replaces consent the subscriber proved. Attest that number instead, which is recorded and leaves it `self_verified`, or have the subscriber verify the new one. `source` is an optional note of up to 500 bytes on where consent came from (`"opted in at the counter"`). `attestedAt` is an optional RFC 3339 time when consent was given; one before 2000-01-01 or in the future is `400`. Attesting again replaces the earlier attestation.

```json
{ "id": "01J…", "subscriberId": "cust-1", "phone": "+15551112222", "phoneConsent": "operator_attested",
  "apiKeyId": "9f86d0…", "source": "opted in at the counter", "attestedAt": "2025-06-01T10:00:00.000000000Z",
  "policyUrl": "https://shop.example/privacy", "createdAt": "2026-11-12T12:00:00.000000000Z" }
```

`403` with code `sms_consent_disabled` means the environment setting is off; `403` with code `forbidden` means the key lacks the permission.

An attestation stops counting when it is revoked, when the subscriber's phone changes to another number, or when the subscriber is deleted. Revoking leaves a self-verified phone verified. A self-verified phone reads as `self_verified` even when it is also attested.

Every attestation is kept for audit and is not deleted when it stops counting. The record holds the subscriber, the phone, the attesting key's id (`apiKeyId`, the SHA-256 of the key, kept after the key is rotated), the time, `source`, `attestedAt` and the policy URL in force at the time. Once the attestation stops counting, the record also holds when and why: `revoked` or `superseded`, with the key that did it and any `reason` it gave, or `phone_changed` or `subscriber_deleted`, which name no key. The change that ends an attestation and its audit record are written in one transaction.

Attested consent does not bypass preferences: the subscriber's own `sms` preference and the step's `defaultEnabled` still decide, as for a self-verified phone.

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
| `HELP`, `INFO` | notify replies through the same integration with `help_text`, or a default naming the number, at most once per number per integration every 10 minutes (a reply that fails to send does not use up that slot). The reply goes to the sender even if their phone is not verified, since it answers them. It is recorded as an `sms` message. | none |

Keywords never change [preferences](../preferences). The opt-out record alone stops sending, so `STOP` then `START` leaves a subscriber exactly as before: their own `sms` preference, and each workflow's `defaultEnabled`, decide as they did. The text and its opt change are stored together: if either fails, notify answers `500` and keeps neither. Twilio does not retry inbound message webhooks by default, so a `STOP` that hit such a failure is not re-sent; turn on Twilio's Advanced Opt-Out (SignalWire has an equivalent opt-out handling) as a backstop so the carrier-side opt-out holds either way. A provider's retry, where it does retry, of a text already handled changes nothing again, so a late retry of an old `STOP` cannot undo a newer `START`.

`help_text` cannot itself be a keyword (`422`), so a HELP reply never reads as a command. Set it to an empty string (or `null`) in `PUT /v1/integrations/{id}`, or empty the field in the console, to go back to the default; leaving it out keeps it.

An opted-out number gets no sms step: it is skipped with `skipReason: "opted_out"`, checked before preferences. The opt-out belongs to the number, so it holds for every workflow, including `critical` ones, which bypass preferences but not an opt-out, and for every subscriber with that number, including one added later. No preference can override it. Only `START` lifts it.

We recommend turning Twilio's Advanced Opt-Out (or SignalWire's equivalent) on. It (and the carriers) may also handle STOP themselves and stop delivering to the number. The text still reaches notify, which records the opt-out as above, so notify's view stays right and the skipped step says why rather than failing at the provider.

### Delivery status

A status callback updates the `sms` message notify sent with that `MessageSid` (kept on the message as `ProviderMessageID`):

| Provider status | Message |
|---|---|
| `accepted`, `queued`, `sending`, `sent` | Stays `sent`. |
| `delivered` | `delivered`. |
| `undelivered`, `failed` | `error`, with `ErrorText` naming the status and `ErrorCode`, and an `ErrorClass` from the code: `recipient` for 30003-30006, 21211, 21610, 21612, 21614 (unreachable, blocked, unknown, landline or unsubscribed numbers); `rate_limited` for 30001; `auth` for 30002; `provider_down` for 30008 and 30500; `rejected_message` otherwise. |

`delivered` and `error` are final. A repeated or late callback (a `sent` after `delivered`, a second `failed`) changes nothing, so callbacks may arrive in any order. A move to `error` counts once against the integration's delivery health; `recipient` failures never do, as elsewhere. Each change is sent to your webhook endpoints as an `sms.status` event. A callback can beat the send's own message row: one for a provider id notify has not stored yet is kept for an hour and applied when that message is written (the message is then written with the final status, delivery health counts it then, and the `sms.status` event is sent then too). Expired ones are purged every 15 minutes. After an hour it is dropped, as is one for a message notify never sent.

## Legacy create

`POST /v1/integrations/sms` takes `provider` (`twilio`, `signalwire`, `sns`), `identifier`, `primary`, `from`, and `accountSid`, `authToken`; `projectId`, `token`, `spaceUrl`; or `accessKeyId`, `secretAccessKey`, `region`.

## Step options

`body`, `integration`, `defaultEnabled`. See the [sms step](../../workflows/step-types/channel-steps#sms).
