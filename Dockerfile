# syntax=docker/dockerfile:1
#
# Multi-target build for the notify stack:
#   --target notify   : the notify server (pure-Go, CGO disabled -> static)
#   --target dagnats  : the dagnats workflow sidecar (fetched via `go install`)
#
# Both run on a small alpine runtime (shell + busybox wget so compose/k8s health
# checks work) as a non-root user, with /data as the persistence volume.
# The DuckDB analytics backend (-tags duckdb) needs CGo and is intentionally not
# built here; the default image stays pure-Go and statically linked.

# ---- notify builder ------------------------------------------------------
FROM golang:1.27.3 AS notify-builder
WORKDIR /src
COPY . .
ARG VERSION=dev
# Same stamping recipe as scripts/build-release.sh, which is what the release
# workflow ships; keep the -ldflags in step with it.
# CGO disabled => fully static binary; the duckdb-tagged code is excluded, so no
# C toolchain or DuckDB bindings are pulled.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/danmestas/notify/internal/version.Version=${VERSION}" \
    -o /out/notify ./cmd/notify

# ---- dagnats builder -----------------------------------------------------
FROM golang:1.27.3 AS dagnats-builder
# dagnats v0.0.24 requires go >= 1.27.1.
# Matches the CI sidecar pin. Do not build against a moving branch ref: it makes
# Docker smoke non-reproducible and can fail transiently at module verification.
ARG DAGNATS_VERSION=v0.0.30
WORKDIR /src
RUN CGO_ENABLED=0 go install github.com/danmestas/dagnats/cmd/dagnats@${DAGNATS_VERSION}

# ---- notify runtime ------------------------------------------------------
FROM alpine:3.20 AS notify
RUN apk add --no-cache ca-certificates tini && \
    addgroup -S notify && adduser -S -G notify -h /home/notify notify && \
    mkdir -p /data && chown notify:notify /data
COPY --from=notify-builder /out/notify /usr/local/bin/notify
USER notify
ENV NOTIFY_ADDR=0.0.0.0:3000 \
    NOTIFY_DB=/data/notify.db
EXPOSE 3000
VOLUME ["/data"]
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/notify"]
CMD ["serve"]

# ---- notify hub ----------------------------------------------------------
# Public-facing variant of `notify`: identical static binary + alpine runtime,
# but fronted by deploy/notify-prelude.sh (env normalization, file-secret
# materialization, provider summary) instead of running notify directly.
# Built by deploy/compose.hub.yml (`--target hub`). Reuses the `notify` stage
# wholesale — no rebuild of the Go binary.
FROM notify AS hub
USER root
COPY deploy/notify-prelude.sh /usr/local/bin/notify-prelude.sh
RUN chmod 0755 /usr/local/bin/notify-prelude.sh
USER notify
# tini (PID 1) reaps zombies + forwards signals; the prelude exec's notify so
# SIGTERM reaches the server for a clean SQLite close.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/notify-prelude.sh"]
CMD []

# ---- dagnats runtime -----------------------------------------------------
FROM alpine:3.20 AS dagnats
RUN apk add --no-cache ca-certificates tini && \
    addgroup -S dagnats && adduser -S -G dagnats -h /home/dagnats dagnats && \
    mkdir -p /data && chown dagnats:dagnats /data
COPY --from=dagnats-builder /go/bin/dagnats /usr/local/bin/dagnats
USER dagnats
ENV DAGNATS_HTTP_ADDR=0.0.0.0:8080 \
    DAGNATS_DATA_DIR=/data
# 4222: NATS client port  8080: HTTP control plane + worker bridge + /ready
EXPOSE 4222 8080
VOLUME ["/data"]
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/dagnats"]
CMD ["serve"]
