---
title: Templating
weight: 3
---

Text fields in a step (`subject`, `body`, `topic`, `url`, `text`, and so on) are [Handlebars](https://handlebarsjs.com/) templates, rendered by [raymond](https://github.com/aymerick/raymond) when the step runs. The full language is available: `{{#if}}`, `{{#each}}`, `{{#with}}`, partials and helpers.

```handlebars
Hi {{subscriber.firstName}},
{{#if payload.items}}You bought:
{{#each payload.items}}- {{this.name}} x{{this.qty}}
{{/each}}{{/if}}
```

A missing path renders as an empty string, not an error. Numbers and booleans render in their natural form. A template that does not parse is not an error either: it renders as its own literal source text, so check a new template with a test send.

## Variables

| Name | Contents |
|---|---|
| `subscriber` | The recipient: `subscriberId`, `firstName`, `lastName`, `email`, `phone`, `locale`, `timezone`, and `data` (the custom object, for example `{{subscriber.data.plan}}`). |
| `payload` | The `payload` object from the trigger. |
| `steps` | Output of earlier **action** steps, keyed by `stepId`. An `http_request` step named `check` gives `{{steps.check.statusCode}}`; an `agent_message` step gives `conversationId`, `activityId`, `runId`. |
| `digest` | After a [digest](../step-types/digest) step only: `digest.count` and `digest.events`. |
| `transactionId` | Available in `agent_message` steps. |

`http_request` templates see only `subscriber` and `payload`, not `steps`.

The values reflect the run's context. After a delay, templates still see the full context.

## Escaping

notify escapes inserted values for where they will land, so a value containing `<` or a quote cannot break an HTML email or a JSON body. Your template text is never escaped, only the values you insert.

| Destination | Escaping |
|---|---|
| Email subject, plain-text email body, push, SMS, in-app, Discord, stream, agent text, URLs | none (values go in as sent) |
| HTML email body (`editorType: "html"` or `htmlBody`), Telegram messages | HTML escaping |
| Slack messages | only the characters Slack's mrkdwn reserves |
| `http_request` headers | JSON string escaping |
| `http_request` body | JSON string escaping when the body is JSON, otherwise none |

You do not need triple braces (`{{{x}}}`) to avoid double escaping. To insert a value exactly as sent, because the field already is markup for the destination (HTML your service rendered from Markdown, say), use the `raw` helper with the path as a string: `{{raw "payload.html"}}`.

## Translations

`{{t "key"}}` looks the key up in the translations for the subscriber's `locale` (`en` when the subscriber has none). A key with no translation renders as the key itself, so an unseeded key is visible rather than blank.

Manage translations with `/v1/translations` or the console. A locale is a map of key to string.

```bash
curl -X PUT localhost:3000/v1/translations/de \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"greeting": "Hallo", "farewell": "Tschüss"}'
```

## Layouts

A **layout** is a template that wraps a step's rendered body. The body is available inside the layout as `{{content}}`, already rendered and not escaped again, so it keeps its markup.

```bash
curl -X POST localhost:3000/v1/layouts \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{"key": "brand", "name": "Brand", "content": "<div class=\"card\">{{content}}<p>Sent by Acme</p></div>"}'
```

Select it per step with `"layoutKey": "brand"` on an `email` or `in_app` step. An empty `layoutKey`, or a key that does not exist, leaves the body unwrapped. Layouts do not apply on their own: a step must name one.
