---
title: Channels and providers
weight: 5
---

A **channel** is how a message reaches a person: `in_app`, `email`, `push`, `chat`, `sms`. (`stream` is a feed, not a person; see [Streams](../streams-and-webhooks).) `in_app` needs no setup. The others send through an **integration**, which is a provider plus its credentials.

{{< cards cols="2" >}}
  {{< card link="email" title="Email" subtitle="SMTP, sendmail, Resend, SES, Cloudflare, webhook" >}}
  {{< card link="push" title="Push" subtitle="ntfy, Expo, FCM, Web Push, webhook" >}}
  {{< card link="chat" title="Chat" subtitle="Discord, Telegram, Slack, WhatsApp, Mastodon, Bluesky, webhook" >}}
  {{< card link="sms" title="SMS" subtitle="Twilio, SignalWire, Amazon SNS" >}}
  {{< card link="preferences" title="Preferences" subtitle="Subscriber opt-outs and digest windows" >}}
{{< /cards >}}

## Integrations

Create one with the unified endpoint:

```bash
curl -X POST localhost:3000/v1/integrations \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{
    "providerId": "resend",
    "identifier": "transactional",
    "primary": true,
    "credentials": {"api_key": "re_…", "from": "Acme <noreply@acme.test>"}
  }'
```

| Field | Meaning |
|---|---|
| `providerId` | The provider. The valid values, by channel, are on the channel pages. An unknown value is a `400` that lists them. |
| `identifier` | Your name for this integration, used by a step's `integration` field. Lowercase letters, digits, hyphens and underscores, starting with a letter or digit (`422` otherwise). |
| `primary` | Prefer this integration for its channel. |
| `credentials` | String fields; the names depend on the provider. |

The response is `201` with `id`, `providerId`, `channel`, `identifier`, `active`, `primary`. Credentials are never returned.

**Legacy endpoints.** `POST /v1/integrations/email`, `/push`, `/chat` and `/sms` are older, channel-specific forms of create, kept for compatibility. They take a `provider` field and camelCase credential names (`apiKey`, `botToken`, `webhookUrl`, …). New code should use `POST /v1/integrations`.

### Which integration sends

When a step has no `integration`, notify uses the channel's **active** integrations, preferring the one marked `primary`, then the lowest `priority`, then the lowest id. A step with `"integration": "transactional"` uses that identifier instead. Several providers can be active for one channel at once. Deactivate one with `PATCH /v1/integrations/{id}/active` and `{"active": false}` (`204`).

### Reading and changing

- `GET /v1/integrations`, `GET /v1/integrations/{id}`: each returns `id`, `providerId`, `channel`, `identifier`, `active`, `primary`, `priority` and `credentialsMasked`.
- `PUT /v1/integrations/{id}` with `identifier`, `primary`, `priority` and `credentials`. Send the mask value `••••••` (or leave a secret field out) to keep a stored secret unchanged; only changed, non-empty values are re-encrypted.
- `DELETE /v1/integrations/{id}`: `204`.

`credentialsMasked` shows non-secret fields as stored and replaces secret fields with `••••••`. A provider with no declared secret fields is masked completely.

### Encryption

Credentials are JSON, sealed with AES-256-GCM under the master key before they are stored, and opened only when a message is sent. If you lose the master key, you lose the credentials. See [Security](../operations/security).

### Network safety

Providers that call a URL you supply (the webhook providers, ntfy, Mastodon, Bluesky) go through the SSRF-safe client described under [HTTP request](../workflows/step-types/http-request#network-restrictions), including the `NOTIFY_SSRF_ALLOWED_HOSTS` allowlist.

## Subscriber addresses

Email goes to `subscriber.email`; SMS to `subscriber.phone`. Push and chat providers need their own address per subscriber, stored with:

```bash
curl -X PUT localhost:3000/v1/subscribers/user-1/credentials \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"providerId": "telegram", "channel": "chat", "credentials": {"address": "123456789"}}'
```

The body is `providerId`, `channel` and a `credentials` object. The address field depends on the provider; see [push](push#addressing) and [chat](chat#addressing). It answers `204`. The path segment is your `subscriberId`.
