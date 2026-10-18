---
title: Operations
weight: 12
---

Running notify for real.

{{< cards cols="2" >}}
  {{< card link="deploy" title="Deploy" subtitle="Native under launchd or systemd, Docker Compose, and the hub compose file" >}}
  {{< card link="dagnats-sidecar" title="The dagnats sidecar" subtitle="What it is, how notify connects, auth, and versions" >}}
  {{< card link="blob-storage" title="Blob storage" subtitle="The fs and s3 adapters, with Garage" >}}
  {{< card link="backups-and-retention" title="Backups and retention" subtitle="What to back up and what is purged" >}}
  {{< card link="observability" title="Observability" subtitle="Health, readiness, metrics, tracing, analytics" >}}
  {{< card link="security" title="Security" subtitle="The master key, rotation, the SSRF guard" >}}
  {{< card link="releases" title="Releases and upgrades" subtitle="Tags, artifacts, and upgrade notes" >}}
{{< /cards >}}

## The shape of a deployment

- **One notify process per database.** State is a SQLite file, a single writer. Do not run two notify processes on one `NOTIFY_DB`; scaling out is a datastore change, not a deployment change.
- **One dagnats sidecar** that notify can reach over NATS and HTTP.
- **A master key** that never changes unless you re-key.
- Optionally a **blob store** and an **MX/SMTP** path for inbound mail.
- Something in front for TLS if you expose the API, such as a reverse proxy or a tunnel. notify itself serves plain HTTP.
