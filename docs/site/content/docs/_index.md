---
title: Documentation
cascade:
  type: docs
---

notify is a self-hosted notification and durable-communications engine. You describe what should happen as a workflow, send it an event, and notify runs the steps: it renders templates, respects subscriber preferences, delivers through email, SMS, push, chat and in-app channels, waits, batches, and calls your HTTP endpoints. It also hosts a subscriber inbox, conversations with agents, and a reply loop for inbound email.

This documentation describes release {{< param "version" >}}. Everything here was written from the source; where behaviour depends on a default or a limit, the page gives the value.

## How it fits together

notify is one Go binary. It keeps its state in a SQLite file and runs workflows on a **dagnats sidecar**, a separate process that provides the durable workflow engine, an embedded NATS server with JetStream, and an HTTP worker bridge. notify connects to the sidecar on start; it never starts it.

```mermaid
graph LR
    Client[Your backend] -->|"/v1 API (ApiKey)"| Notify[notify]
    Browser[Subscriber browser] -->|"/v1 inbox (Bearer session)"| Notify
    Notify --> DB[(SQLite)]
    Notify <-->|NATS + HTTP bridge| Dagnats[dagnats sidecar]
    Notify --> Providers[Email, SMS, push, chat providers]
    Notify -.->|optional| Blobs[(fs or S3 blob store)]
    Mail[Mail server or forwarder] -->|inbound email| Notify
```

## Where to go next

{{< cards cols="2" >}}
  {{< card link="get-started" title="Get started" subtitle="Run notify, mint an API key, create a workflow, trigger it" >}}
  {{< card link="concepts" title="Concepts" subtitle="Scopes, subscribers, topics, workflows, integrations, runs" >}}
  {{< card link="workflows" title="Workflows and step types" subtitle="Everything a workflow can do" >}}
  {{< card link="api" title="API reference" subtitle="Every /v1 route" >}}
  {{< card link="configuration" title="Configuration" subtitle="Every flag and environment variable" >}}
  {{< card link="operations" title="Operations" subtitle="Deploy, the dagnats sidecar, blob storage, backups" >}}
{{< /cards >}}
