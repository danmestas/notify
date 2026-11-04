---
title: A2A client keys
weight: 11
---

A client key lets a machine caller (another agent or a service) call notify's A2A channel. It is per environment, named, and optionally bound to one subscriber, so the caller speaks as that person. The secret is shown once at mint and stored only as a SHA-256 hash; it cannot be recovered, so mint a new key if it is lost.

| Method and path | Auth | Purpose |
|---|---|---|
| `POST /v1/a2a/client-keys` | API key | Mint `{"name", "subscriberId"?}`. `201` with the key and its `secret`, shown once. |
| `GET /v1/a2a/client-keys` | API key | List the environment's keys, revoked ones included: `{data: […]}`. |
| `DELETE /v1/a2a/client-keys/{id}` | API key | Revoke. `204`; `404` when the id is unknown or already revoked. |

A key: `{id, name, subscriberId?, createdAt, lastUsedAt?, revokedAt?}`. The mint response adds `secret`, which looks like `a2ack_<64 hex>`. The `a2ack_` prefix is how notify tells a client key from a subscriber session token. `name` is required (at most 64 characters); a `subscriberId` that does not exist in the environment is `422`. Responses carry `Cache-Control: no-store`.

You can also list, mint (with a copy button on the one-time secret) and revoke keys on the console's **A2A Client Keys** page.

## Calling with a key

The A2A routes accept `Authorization: Bearer <token>`, where the token is either a client key or a [subscriber session](../../inbox-and-agents/subscriber-sessions). Both resolve to the same caller: the organization and environment, the subscriber if any, and the credential id. A key works only in its own environment.

Tenant API keys are not accepted on the A2A routes, and client keys are not accepted anywhere else in the API. A missing, malformed, unknown or revoked credential is `401` with a plain-text `unauthorized` body.

## Audit

Minting, revoking and every use of a credential is written to the log with the credential id, environment and kind. The token is never logged. Each key also records `lastUsedAt`.

## Rotating the API key

Client keys are separate credentials and survive an API key rotation. Anyone who held the old API key could have minted client keys, so after rotating a key you suspect leaked, list the env's client keys and revoke any you don't recognise.
