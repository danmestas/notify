---
title: Drafts
weight: 5
---

A **draft** is an agent's reply to an email, held for the mailbox owner to review and send. notify never sends it.

Drafts are produced by an [`agent_message`](../../workflows/step-types/agent-message) step with `reviewDraft: true`, typically through the [`inbound-mail` workflow template](../../workflows/templates). The trigger payload names the mailbox and message; when the agent answers, the reply is stored as a draft instead of being posted back.

## Fetching drafts

```bash
curl "localhost:3000/v1/inbox/drafts?subscriberId=user-1&mailbox=acct-1&mailbox=acct-2&limit=50" \
  -H "Authorization: ApiKey $KEY"
```

| Query | Meaning |
|---|---|
| `subscriberId` | Required. |
| `mailbox` | Required, repeatable. A caller names exactly the mailboxes it may see. |
| `after` | The `cursor` from the previous response. Empty starts from the oldest draft. |
| `limit` | Default 50, maximum 200. |

Without `subscriberId` and at least one `mailbox` the answer is `400`. Drafts come oldest first.

```json
{
  "drafts": [{
    "type": "agent.draft.proposed",
    "draftId": "01JDRAFT00000000000000000A",
    "agentIdentifier": "support",
    "agentName": "Support",
    "conversationId": "th-1",
    "activityId": "x1",
    "subscriberId": "s1",
    "mailbox": "acct-1",
    "sourceMessageId": "msg-7",
    "inReplyTo": "<orig@example.com>",
    "references": ["<root@example.com>", "<orig@example.com>"],
    "subject": "Re: Invoice",
    "to": ["ann@example.com"],
    "text": "Thanks, **paid** today.",
    "createdAt": "2026-10-17T12:00:00.000Z"
  }],
  "cursor": "42.1792280000000",
  "gap": false
}
```

| Field | Meaning |
|---|---|
| `draftId` | The draft's id. |
| `agentIdentifier`, `agentName` | The agent that wrote it. |
| `conversationId`, `activityId` | The conversation thread and the agent entry the draft came from. |
| `mailbox`, `sourceMessageId` | The message being answered, by the mailbox's own id. |
| `inReplyTo`, `references`, `subject`, `to` | **Advisory.** The mailbox owner recomputes recipients and threading itself. |
| `text` | The reply; may be Markdown. |

`cursor` is opaque: pass it back as `after`. When there are no more drafts it equals the cursor you sent.

## Retention and gaps

Drafts are kept for **30 days**, and a purge runs at start-up and hourly. If your `after` cursor is older than that, drafts created after it may have expired before you fetched them, and the response sets `"gap": true`. Treat that as a signal to resynchronize rather than as an error.
