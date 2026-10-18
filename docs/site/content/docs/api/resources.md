---
title: Other resources
weight: 8
---

All routes take an API key and return the JSON error envelope.

## Layouts

Wrappers for email and in-app bodies; see [Templating](../../workflows/templating#layouts).

| Method and path | Purpose |
|---|---|
| `GET /v1/layouts?after=<key>&limit=<n>` | List: `{layouts, nextKey?}`. |
| `POST /v1/layouts` | Create `{key, name, content}`; `key` and `name` required; `409` if the key exists. `201`. |
| `GET /v1/layouts/{key}` | One layout. `404`. |
| `PUT /v1/layouts/{key}` | Upsert `{name, content}`. |
| `DELETE /v1/layouts/{key}` | Delete. `204`. |

A layout: `{id, key, name, content, orgId, envId}`.

## Translations

| Method and path | Purpose |
|---|---|
| `GET /v1/translations` | Locales with key counts: `{locales: [{locale, keyCount}]}`. |
| `POST /v1/translations` | Upsert one key: `{locale, key, value}`. `201`. |
| `GET /v1/translations/{locale}` | The locale's whole map. `404` if it has no keys. |
| `PUT /v1/translations/{locale}` | Replace the whole map: a JSON object of key to string. `200 {locale, keyCount}`. |
| `PUT /v1/translations/{locale}/{key}` | Upsert one key: `{value}`. |
| `DELETE /v1/translations/{locale}/{key}` | Delete one key. `204`; `404`. |

## Contexts

Typed records with JSON `data`, keyed by `type` and `id`.

| Method and path | Purpose |
|---|---|
| `GET /v1/contexts?type=&search=&after=&before=&limit=` | List: `{data, nextCursor?, prevCursor?}`. |
| `POST /v1/contexts` | Create `{type, id, data}`; all required. `409` if it exists. `201`. |
| `GET /v1/contexts/{type}/{id}` | One context. `404`. |
| `PUT /v1/contexts/{type}/{id}` | Replace `data`. |
| `DELETE /v1/contexts/{type}/{id}` | Delete. `204`. |

A context: `{id, type, externalId, data, createdAt, updatedAt}`.

## Environment variables

Per-environment key/value pairs. Keys are letters, digits, hyphens and underscores, starting with a letter or digit; uppercase is allowed. Values are encrypted at rest.

| Method and path | Purpose |
|---|---|
| `GET /v1/env-vars?search=&after=&before=&limit=&reveal=1` | List: `{items, nextCursor?, prevCursor?}`. Values are masked unless `reveal=1`. |
| `POST /v1/env-vars` | Create or upsert `{key, isSecret, value}`. `201`, masked. |
| `PUT /v1/env-vars/{key}` | Update `{isSecret?, value}`. An empty or masked `value` keeps the stored one. |
| `DELETE /v1/env-vars/{key}` | Delete. `204`. |

An env var: `{id, key, isSecret, value, createdAt, updatedAt}`.

Contexts and environment variables are stored and managed here and in the console. The workflow templating engine does not read either of them.
