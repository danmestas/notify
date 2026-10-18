---
title: Workflow templates
weight: 5
---

notify ships workflows that are ready to apply. Today there is one.

```bash
curl -s localhost:3000/v1/workflow-templates -H "Authorization: ApiKey $KEY"
curl -s -X POST localhost:3000/v1/workflow-templates/inbound-mail -H "Authorization: ApiKey $KEY"
```

`GET /v1/workflow-templates` lists them, with `name`, `description`, `triggerIdentifier` and `steps`. `POST /v1/workflow-templates/{name}` creates the template's workflow in your environment and publishes it:

- `201`, with `"outcome": "created"`, when it creates the workflow.
- `200`, with `"outcome": "unchanged"`, when a workflow with that trigger identifier already exists. **It is left exactly as it is**, whether you edited it or not; applying a template never overwrites your workflow.
- `404` for a name that is not a template.

## inbound-mail

*Answer each inbound email with a draft from the agent the mailbox chose, for the owner to review and send.*

Trigger identifier `inbound-mail`, name `Inbound mail drafts`, one step:

```json
{
  "stepId": "draft",
  "type": "agent_message",
  "controlValues": {
    "agent": "{{payload.agent}}",
    "text": "New email from {{payload.from}}\nSubject: {{payload.subject}}\n\n{{payload.text}}",
    "conversation": "{{payload.mailbox}}:{{payload.messageId}}",
    "failurePolicy": "fail_closed",
    "reviewDraft": true
  }
}
```

It is for a mail client that has its own mailbox and wants an AI agent's suggested reply. The client triggers `inbound-mail` for each new message with a payload of:

| Field | Meaning |
|---|---|
| `mailbox` | The mailbox the message arrived in. |
| `messageId` | The mailbox's own id for the message. |
| `rfcMessageId` | The message's `Message-ID` header. |
| `references` | Earlier `Message-ID`s in the thread. |
| `subject`, `from` | From the message. |
| `text` | The message body. Share it only for mailboxes that have an agent. |
| `agent` | The identifier of the agent that should answer. A mailbox with no agent sends an empty `agent`, and the step is skipped for that event without failing. |

The agent's reply is not sent. It becomes a **draft** that the mailbox owner collects from [`GET /v1/inbox/drafts`](../../inbox-and-agents/drafts) and decides whether to send. See [`agent_message`](../step-types/agent-message) for how `reviewDraft` works, and [Drafts](../../inbox-and-agents/drafts) for the draft format.
