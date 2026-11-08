---
title: Subscriber sessions
weight: 2
---

An API key is tenant-wide: a page holding one could read every subscriber's messages by changing a query parameter. So a browser must not hold it. A **subscriber session** is a token that authenticates exactly one subscriber's inbox.

## Mint a token

Your backend, after it has authenticated the user, calls:

```bash
curl -X POST localhost:3000/v1/subscribers/user-1/session -H "Authorization: ApiKey $KEY"
```

```json
{ "token": "…", "expiresAt": "2026-10-18T13:00:00Z" }
```

The response carries `Cache-Control: no-store`. The subscriber must already exist (`404` otherwise), so a token never authenticates a permanently empty inbox. The token is valid for **one hour** and is stateless: there is no revocation list, so expiry is the only thing that ends it. Mint a new one for a logged-in user as needed.

## Use it

```bash
curl "localhost:3000/v1/messages" -H "Authorization: Bearer $TOKEN"
```

The token is accepted only by the four inbox message routes (`GET /v1/messages`, `GET /v1/messages/count`, `POST /v1/messages/mark`, `POST /v1/messages/{id}/{action}`) and the two [phone verification](#verify-a-phone) routes, and nowhere else. The restriction is structural, not per-handler: no other route sits behind middleware that accepts a Bearer token.

With a session, the token decides whose inbox it is. A `subscriberId` in the query or body is replaced by the token's, never compared, so a client cannot widen its own access by naming someone else. The `integrationId` listing is refused with `403`.

The live stream `GET /v1/inbox/stream` and everything else under `/v1` take an API key only.

## Verify a phone

[SMS steps](../../workflows/step-types/channel-steps#sms) text only a phone the subscriber verified, and only the subscriber can verify one: these two routes take a session token and refuse an API key with `403`. The token decides whose phone it is; nothing in the body can name another subscriber.

Send a code to a number:

```bash
curl -X POST localhost:3000/v1/me/phone/verification -H "Authorization: Bearer $TOKEN" \
  -d '{"phone":"+1 555 123 4567"}'
```

```json
{ "phone": "+15551234567", "expiresAt": "2026-10-18T12:10:00Z" }
```

The number is normalised to E.164 (spaces, dots, dashes and parentheses are dropped; it must start with `+` and a country code). A six-digit code is texted to it through the environment's primary SMS integration. Then confirm:

```bash
curl -X POST localhost:3000/v1/me/phone/verification/confirm -H "Authorization: Bearer $TOKEN" \
  -d '{"code":"123456"}'
```

```json
{ "phone": "+15551234567", "verifiedAt": "2026-10-18T12:01:12Z" }
```

That number becomes the subscriber's verified phone, replacing any phone they had. Only the newest code counts, and only once.

| Status | `error.code` | Meaning |
|---|---|---|
| `202` | | Code sent (request). |
| `200` | | Phone verified (confirm). |
| `403` | `subscriber_session_required` | Called with an API key. |
| `422` | `invalid_phone` | Not an E.164 number. |
| `429` | `rate_limited` | 5 codes in the last hour for this subscriber, or for this number across subscribers. |
| `502` | `sms_send_failed` | The SMS integration refused or failed the send. |
| `409` | `no_pending_code` | No unused code to confirm. |
| `410` | `code_expired` | The code is more than 10 minutes old. Request a new one. |
| `422` | `wrong_code` | The code does not match. |
| `429` | `too_many_attempts` | 5 wrong codes; this code is spent. Request a new one. |

Only a salted hash of each code is stored. Each send is recorded as an `sms` message on the integration (without the code), so it counts toward the integration's delivery health like any delivery.

The sender API can still write `phone` on `POST` and `PATCH /v1/subscribers`, but that phone is stored unverified, and writing a different number clears a verification. `GET /v1/subscribers/{id}` shows `phoneVerified`.
