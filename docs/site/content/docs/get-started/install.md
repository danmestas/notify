---
title: Install
weight: 1
---

notify runs as two processes: the **notify** server and a **dagnats** sidecar that notify connects to. Install both.

## notify

### Release binary

Pushing a tag named `version-<x.y.z>` to the repository runs the release workflow, which builds static binaries (`CGO_ENABLED=0`) for `linux/amd64` and `darwin/arm64` and attaches them to a GitHub release as `notify-<version>-<os>-<arch>.tar.gz`, with a `SHA256SUMS` file. Each tarball holds one `notify` binary. The release notes are the annotation of the tag, so they list any new or newly enforced environment variable.

```bash
sha256sum -c SHA256SUMS            # verify the download
tar xzf notify-0.3.3-linux-amd64.tar.gz
./notify-0.3.3-linux-amd64/notify version
```

A binary stamped by the release script reports its version. A binary built without the stamp reports `0.0.0-dev`.

### From source

The module requires Go 1.27.3 or newer.

```bash
go build -trimpath -o notify ./cmd/notify
```

To stamp a version the way the release does:

```bash
scripts/build-release.sh 0.3.3 linux/amd64     # writes dist/notify-0.3.3-linux-amd64.tar.gz
```

The default build is pure Go and has no C dependencies. An optional DuckDB analytics backend needs cgo and a build tag; see [Analytics backend](../../operations/observability#analytics-backend).

### Subcommands

```text
notify serve           run the API server
notify bootstrap       create an organization, an environment and an API key
notify rotate-key      replace the API key of an existing environment
notify validate-steps  check stored workflows against the current step contracts
notify inbound         pipe mode for inbound email (reads a message on stdin)
notify version         print the version
```

`notify` with no arguments prints the version and a usage line. The flags and environment variables of each subcommand are in the [configuration reference](../../configuration).

## dagnats

dagnats provides the durable workflow engine, an embedded NATS server with JetStream, and the HTTP worker bridge that notify polls for tasks. notify never starts it; start it first.

```bash
go install github.com/danmestas/dagnats/cmd/dagnats@v0.0.34
dagnats serve
```

The `Dockerfile` and CI pin the dagnats version (`DAGNATS_VERSION`, currently `v0.0.34`); use the same one. With no configuration `dagnats serve` binds NATS on `127.0.0.1:4222` and its HTTP control plane and worker bridge on `127.0.0.1:8080`, which are the defaults notify expects (`-nats-url`, `-bridge-url`). Its data directory defaults to a per-user path, or `DAGNATS_DATA_DIR` if set.

Read [the dagnats sidecar](../../operations/dagnats-sidecar) before running this anywhere that is not your laptop.
