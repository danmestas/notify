---
title: Integrations
weight: 3
---

All routes take an API key. Concepts and per-provider credentials are in [Channels](../../channels).

| Method and path | Purpose | Success |
|---|---|---|
| `POST /v1/integrations` | Unified create: `{providerId, identifier, primary, credentials}`. | `201` `{id, providerId, channel, identifier, active, primary}` |
| `GET /v1/integrations` | List. | `200` array of integrations |
| `GET /v1/integrations/{id}` | One integration. | `200`; `404` |
| `PUT /v1/integrations/{id}` | Update `{identifier, primary, priority, credentials}`. Secrets sent as `••••••` are kept. | `200`; `404`; `409` |
| `DELETE /v1/integrations/{id}` | Delete. | `204`; `404` |
| `PATCH /v1/integrations/{id}/active` | `{"active": bool}`. | `204`; `404` |
| `POST /v1/integrations/email` | Legacy email create. | `201` |
| `POST /v1/integrations/push` | Legacy push create. | `201` |
| `POST /v1/integrations/chat` | Legacy chat create. | `201` |
| `POST /v1/integrations/sms` | Legacy SMS create. | `201` |
| `GET /v1/integrations/webpush/key` | The environment's VAPID public key; mints the pair on first use. | `200` `{publicKey, subject, subjectDefault}` |
| `PUT /v1/integrations/webpush/subject` | Set the VAPID contact: `{"subject"}`. | `200` as above; `400` for an unusable subject |

An integration object:

```json
{ "id": "…", "providerId": "resend", "channel": "email", "identifier": "transactional",
  "active": true, "primary": true, "priority": 0,
  "credentialsMasked": { "api_key": "••••••", "from": "Acme <noreply@acme.test>" } }
```

`providerId` for the unified create is one of `smtp, cloudflare-email, resend, ses, ntfy, expo, fcm, discord, telegram, slack, whatsapp, mastodon, bluesky, twilio, signalwire, sns, email-webhook, push-webhook, chat-webhook`, plus `sendmail`. An unknown one is `400`. A bad `identifier` is `422`. Responses never include credentials in the clear.
