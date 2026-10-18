---
title: Reply loop
weight: 1
---

## How it works

When notify sends an email that expects an answer, it sets Reply-To to a **reply mailbox**:

```text
reply+<token>@<inbound-domain>
```

The token is an HMAC-signed reference to a stored email thread (a binding of the conversation, subscriber and agent or webhook). When mail arrives at that address, notify verifies the token, checks that the domain is still a verified inbound domain for the thread's scope, and routes the body:

- **Agent threads** thread the text onto the agent's conversation and run the agent's turn. The agent's reply is mailed back to the subscriber with a fresh reply address, so the exchange continues.
- **Webhook threads** hand the reply to your environment's [outbound webhooks](../../streams-and-webhooks#outbound-webhooks) as an `email.reply` event. These are created by an email step with `reply: true`.

Quoted reply history ("On … wrote:" and similar markers) is cut off before the text is used. If there is no plain-text part, the HTML part is stripped of tags. An empty body is dropped.

Two ways to start an email-origin agent conversation: `POST /v1/agents/{identifier}/conversations/email` ([see](../../inbox-and-agents/agents#email-conversations)), or an email step with `reply: true` for a webhook thread.

## Inbound domains

Reply addresses live on a domain you point at notify. Register it and verify it.

```bash
curl -X POST localhost:3000/v1/inbound/domains -H "Authorization: ApiKey $KEY" \
  -H 'Content-Type: application/json' -d '{"name": "reply.acme.test"}'
```

The domain is created with status `pending` and an `expectedMXHost` and `expectedMXPriority` (10). Create an MX record for the domain pointing at that host, then:

```bash
curl -X POST localhost:3000/v1/inbound/domains/reply.acme.test/verify -H "Authorization: ApiKey $KEY"
```

Verification does a live MX lookup and sets the status to `verified` if the expected host is among the results, otherwise `failed`. The expected host is `NOTIFY_INBOUND_MX_HOST`; if unset it is the placeholder `inbound.notify.example.com`, so **set it** to your real MX host. A domain can be listed, fetched and deleted too: see the [API reference](../../api/inbound).

Mail is accepted only for verified domains. A reply mailbox is only handed out when the scope has a verified domain, and when inbound is enabled (a forwarder secret is set, or the SMTP receiver is on).

## Three ways in

All three feed the same code, so they cannot disagree about what a message contained or where it went.

### 1. The JSON webhook

`POST /v1/inbound/email` for a mail service that parses messages and forwards them (a Cloudflare Email Worker, Resend inbound). It is authenticated by a shared secret, not an API key:

```bash
curl -X POST localhost:3000/v1/inbound/email \
  -H "X-Inbound-Secret: $NOTIFY_INBOUND_SECRET" -H 'Content-Type: application/json' \
  -d '{"from": "ada@example.com", "to": "reply+TOKEN@reply.acme.test", "subject": "Re: Hello", "text": "Thanks!"}'
```

The body (at most 1 MiB) is an object with `from`, `to`, `subject`, `text`, optional `html`, optional `envelopeTo` (preferred over `to` for finding the token, since forwarders can rewrite `to`), `inReplyTo`, `references`, `headers`, `attachments` (metadata only) and `raw` (the message as base64, kept if a blob store is configured).

| Status | Meaning |
|---|---|
| `200` | Accepted. The body is `{"status": "…"}`; see below. |
| `400` | The body is not valid JSON, or too large. |
| `401` | Wrong or missing `X-Inbound-Secret`. |
| `500` | The responder failed in a way worth retrying; the forwarder should retry. |
| `503` | `NOTIFY_INBOUND_SECRET` is not set. Inbound is closed unless you set it. |

Mail that notify deliberately ignores still answers `200`, with a reason, so the mail service does not retry it: `threaded` (success), `no_token`, `invalid_token`, `domain_unverified`, `no_binding`, `empty_body`, `agent_not_found`. Every drop is logged with the address and reason.

### 2. Pipe mode

If you already run an MTA, hand it to `notify inbound`. In `/etc/aliases`:

```text
reply: "|/usr/local/bin/notify inbound"
```

The command reads one message on stdin, parses it with the same parser as the SMTP receiver, and POSTs it to a running notify's `/v1/inbound/email`. Flags: `-server` (default `NOTIFY_SERVER_URL` or `http://127.0.0.1:3000`), `-secret` (default `NOTIFY_INBOUND_SECRET`), `-timeout` (default 30s). Exit codes follow `sysexits.h`, which is what an MTA reads to choose between bouncing and retrying:

| Exit | Meaning |
|---|---|
| `0` | Delivered, including every intentional drop. |
| `65` | The message will never parse (`EX_DATAERR`), including one over 1 MiB. |
| `75` | Server or network fault; the MTA should queue and retry (`EX_TEMPFAIL`). |
| `78` | Wrong secret, inbound not configured, or a bad URL (`EX_CONFIG`). |

### 3. The embedded SMTP receiver

Start notify with `-smtp 0.0.0.0:25` (or `NOTIFY_SMTP_ADDR`) and make notify itself the MX host. It is not a mail server: every address it accepts is a valid, armed reply mailbox. A recipient that is anything else is refused at `RCPT TO` with `550`, before the body is read, so there is no inbox to fill and no relaying. One recipient per message; messages are capped at 1 MiB. A transient failure answers `451` so the sender retries.

STARTTLS is offered, not required, when you give a certificate: `-smtp-cert` and `-smtp-key` (`NOTIFY_SMTP_CERT`, `NOTIFY_SMTP_KEY`), both or neither. The files are re-read when they change, so a renewal needs no restart. A certificate that cannot be loaded at start is fatal. The SMTP banner names `NOTIFY_INBOUND_MX_HOST`, or the machine's hostname if unset.

notify becomes internet-facing on port 25 when you do this, which is why it is off by default. There is no DKIM or SPF check: the token authenticates the person and the thread.

With the SMTP receiver on, reply mailboxes are handed out even without a forwarder secret. The JSON webhook still needs the secret; the two doors are gated independently.
