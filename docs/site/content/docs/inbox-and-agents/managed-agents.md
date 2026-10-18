---
title: Managed agents
weight: 4
---

A **managed** agent is one notify runs for you: no code of yours, just a model, a system prompt and the conversation so far. Each agent brings its own model settings and API key. A turn is one model call; the turn already runs as a durable engine step, so a failed call is retried and a restart resumes it.

## Creating one

```bash
curl -X POST localhost:3000/v1/agents \
  -H "Authorization: ApiKey $KEY" -H 'Content-Type: application/json' \
  -d '{
    "identifier": "ember",
    "name": "Ember",
    "runtime": "managed",
    "managed": {
      "provider": "openrouter",
      "model": "anthropic/claude-sonnet-4.5",
      "systemPrompt": "Draft replies. Be brief.",
      "maxTokens": 1024,
      "apiKey": "sk-or-…"
    }
  }'
```

## Settings

| Field | Meaning |
|---|---|
| `provider` | Required. Case-insensitive. `anthropic`, or any other name. |
| `model` | Required. The provider's model name. |
| `systemPrompt` | Optional system prompt. |
| `maxTokens` | Output limit per reply, 0 to 8192. `0` or absent means 1024. |
| `baseUrl` | The base address of an OpenAI-compatible API. Optional for a preset provider, required for any other. When set it wins over the preset. Not used for `anthropic`. |
| `apiKey` | The provider's key. **Sealed at rest and never returned.** |

Settings may be left out when you create the agent and added later with `PATCH`; they are validated whenever they are sent (`422` with the reason), and a turn on an agent with no usable settings fails permanently.

### Providers

`anthropic` uses Anthropic's own API. **Every other provider** is reached through the OpenAI-compatible chat API, which most hosted services and local model servers speak. Naming one of these providers is enough, because notify knows its base URL:

| Provider | Base URL |
|---|---|
| `openai` | `https://api.openai.com/v1` |
| `openrouter` | `https://openrouter.ai/api/v1` |
| `groq` | `https://api.groq.com/openai/v1` |
| `together` | `https://api.together.xyz/v1` |
| `mistral` | `https://api.mistral.ai/v1` |
| `deepseek` | `https://api.deepseek.com/v1` |
| `xai` | `https://api.x.ai/v1` |
| `gemini` | `https://generativelanguage.googleapis.com/v1beta/openai` |
| `fireworks` | `https://api.fireworks.ai/inference/v1` |
| `perplexity` | `https://api.perplexity.ai` |
| `ollama` | `http://localhost:11434/v1` |
| `lmstudio` | `http://localhost:1234/v1` |

A preset is used only when you name the provider: nothing is sent to a hosted service by omission. Any other provider name needs a `baseUrl`, for example a vLLM server.

## The API key

- You send `apiKey` in plaintext over `POST` or `PATCH`. notify seals it with the master key before storing it. A server with no master key refuses to store one.
- It is never returned. Responses replace it with `"apiKeySet": true` or `false`.
- On `PATCH`, **leaving `apiKey` out keeps the stored key**, so you can change the prompt without resending it. Sending `""` clears it.
- The key is opened only when a turn runs. If the master key has changed since, the turn fails permanently with a message that the key cannot be opened.

```json
{ "managed": { "apiKeySet": true, "model": "anthropic/claude-sonnet-4.5", "provider": "openrouter", "systemPrompt": "Draft replies." } }
```

## How a turn runs

The transcript becomes model messages: the agent's own entries are the assistant side, everything else the user side. Consecutive messages from one side are joined, because some providers require the sides to alternate, and the conversation always opens on the user side. The last 20 entries are used. notify makes one model call with your system prompt and returns the text as the reply.

A transport or provider error is transient and retried. An empty answer is an error. A bad settings blob, an unopenable key, or no message to answer is permanent.

## Separate from the workflow builder

The AI workflow builder in the console (`NOTIFY_AI_API_KEY`, `ANTHROPIC_API_KEY`) is configured separately and does not affect managed agents; see [Configuration](../../configuration#ai-workflow-builder).
