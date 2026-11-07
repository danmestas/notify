---
title: Integrations
weight: 3
---

All routes take an API key. Concepts and per-provider credentials are in [Channels](../../channels).

| Method and path | Purpose | Success |
|---|---|---|
| `POST /v1/integrations` | Unified create: `{providerId, identifier, primary, credentials}`. | `201` `{id, providerId, channel, identifier, active, primary}` |
| `GET /v1/integrations` | List, each with its `health`. | `200` array of integrations |
| `GET /v1/integrations/{id}` | One integration, with its `health`. | `200`; `404` |
| `PUT /v1/integrations/{id}` | Update `{identifier, primary, priority, credentials}`. Secrets sent as `••••••` are kept. | `200`; `404`; `409` |
| `DELETE /v1/integrations/{id}` | Delete. | `204`; `404` |
| `PATCH /v1/integrations/{id}/active` | `{"active": bool}`. | `204`; `404` |
| `POST /v1/integrations/{id}/verify` | Run the credential check now and return the resulting [health](#health). | `200` health object; `404`; `502` when the check could not be completed |
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
  "credentialsMasked": { "api_key": "••••••", "from": "Acme <noreply@acme.test>" },
  "health": { "state": "failing", "since": "2026-11-07T06:17:03Z",
              "lastErrorClass": "auth", "lastErrorMessage": "resend: credentials rejected (status 401)",
              "lastCheckedAt": "2026-11-07T06:17:03Z" } }
```

`health` appears on `GET /v1/integrations` and `GET /v1/integrations/{id}`; the create and update responses leave it out.

## Health

`health` is the integration's [delivery health](../../operations/observability#delivery-health):

| Field | Meaning |
|---|---|
| `state` | `healthy`, `degraded`, `failing`, or `unverified` (nothing has proven the integration works yet). |
| `since` | When the integration entered `state`. Absent for an integration nothing has been recorded for. |
| `lastErrorClass` | The class of the latest counted error, for example `auth`. Absent until one has happened. |
| `lastErrorMessage` | The provider's message for that error. Never a credential. |
| `lastCheckedAt` | When the credentials were last checked, by the daily check, after a credential change, or by `verify`. Absent if never. |

`POST /v1/integrations/{id}/verify` probes the provider with the stored credentials, without sending a message, and waits for the verdict. A pass clears `failing` and `unverified`; a credential refusal sets `failing`. A provider that has no credential probe, or a provider that cannot be reached, leaves `state` as it was. The response is the health object alone, and the call is scoped like every other: an integration in another environment is `404`. Any valid API key can call it, the same as the other integration routes.

To protect the provider account, an integration is probed at most once every 10 seconds: a verify inside that window returns the stored health without probing, and simultaneous verifies share one probe. A provider's error text is stored with every credential value of the integration replaced by `[redacted]`, and cut to 500 characters.

`providerId` for the unified create is one of `smtp, cloudflare-email, resend, ses, ntfy, expo, fcm, discord, telegram, slack, whatsapp, mastodon, bluesky, twilio, signalwire, sns, email-webhook, push-webhook, chat-webhook`, plus `sendmail`. An unknown one is `400`. A bad `identifier` is `422`. Responses never include credentials in the clear.
