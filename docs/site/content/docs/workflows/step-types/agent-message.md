---
title: Agent message
weight: 7
---

An `agent_message` step wakes an [agent](../../../inbox-and-agents/agents): it posts text into a conversation with that agent and starts the agent's turn, so an event can start (or continue) a conversation.

```json
{
  "stepId": "ask",
  "type": "agent_message",
  "controlValues": {
    "agent": "support",
    "text": "New ticket from {{subscriber.email}}: {{payload.subject}}",
    "conversation": "ticket-{{payload.ticketId}}",
    "failurePolicy": "fail_closed",
    "reviewDraft": false
  }
}
```

## Fields

| Field | Default | Meaning |
|---|---|---|
| `agent` | `""` | Template giving the agent's `identifier`. It can come from the event: `{{payload.agent}}`. |
| `text` | `""` | Template for the message posted to the conversation. Inserted as plain text. |
| `conversation` | `""` | Template for a conversation handle. Messages with the same handle continue one thread; empty starts a new conversation. |
| `failurePolicy` | `fail_closed` | `fail_closed` or `continue`. |
| `reviewDraft` | `false` | When `true`, the agent's reply becomes a **draft** for a human to review and send, instead of a plain reply. See [Drafts](../../../inbox-and-agents/drafts). |

`agent` and `text` may be empty while you are authoring, but a step that still has them empty when it runs fails.

Templates see `subscriber`, `payload`, `transactionId` and `steps`.

## Behavior

- The message is posted as the subscriber of the run, in the same way as the inbox compose API. The agent's turn runs durably on the engine, not inside the step.
- If `agent` has text but **renders to empty** (for example `{{payload.agent}}` on an event that names no agent), the step is an opt-out for that event: it posts nothing and succeeds.
- The post is idempotent. It is keyed by transaction, subscriber and step, so a retried step or a redelivered trigger does not post twice.
- The agent must exist and be **active**. Unknown or inactive agents, a runtime that cannot run on this server, and empty rendered text are failures that follow `failurePolicy`: `fail_closed` fails the run, `continue` saves an empty output and moves on.
- Output saved for later steps: `{"conversationId", "activityId", "runId"}`, readable as `{{steps.ask.conversationId}}`.

## reviewDraft

With `reviewDraft: true` the trigger payload must be an **inbound-mail event**, an object with at least:

| Field | Meaning |
|---|---|
| `mailbox` | Required. The mailbox the message arrived in. |
| `messageId` | Required. The mailbox's own id for the message. |
| `rfcMessageId` | The `Message-ID` header. Becomes `inReplyTo`. |
| `references` | Array of earlier `Message-ID`s for threading. |
| `subject` | Subject; the draft gets `Re: ` prepended unless it already starts with `re:`. |
| `from` | Sender; becomes the draft's `to`. |

Without `mailbox` and `messageId` the step fails (`reviewDraft needs an inbound-mail payload with mailbox and messageId`). The built-in [`inbound-mail` workflow template](../../templates) is exactly this step.
