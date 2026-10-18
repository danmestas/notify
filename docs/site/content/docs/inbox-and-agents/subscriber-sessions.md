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

The token is accepted only by the four inbox message routes (`GET /v1/messages`, `GET /v1/messages/count`, `POST /v1/messages/mark`, `POST /v1/messages/{id}/{action}`), and nowhere else. The restriction is structural, not per-handler: no other route sits behind middleware that accepts a Bearer token.

With a session, the token decides whose inbox it is. A `subscriberId` in the query or body is replaced by the token's, never compared, so a client cannot widen its own access by naming someone else. The `integrationId` listing is refused with `403`.

The live stream `GET /v1/inbox/stream` and everything else under `/v1` take an API key only.
