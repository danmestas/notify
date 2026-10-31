#!/usr/bin/env bash
#
# notify-prelude.sh — container entrypoint for the notify comms hub.
#
# Mirrors darken's images/claude/darkish-prelude.sh: it runs BEFORE the long-
# lived process, normalizes/validates the notify environment, materializes any
# file-mounted secrets into the *_B64 form the app expects, logs which providers
# are wired (names only — never values), then exec's `notify serve` as PID 1's
# child so signals propagate cleanly.
#
# Design notes:
#   * Engine NATS (NOTIFY_NATS_URL) is the in-pod dagnats
#     sidecar reached over loopback. MESH NATS (NOTIFY_MESH_NATS_URL) is the
#     EXTERNAL sesh A2A hub where responder agents (pi, gemini, omp, grok)
#     subscribe. They are deliberately different endpoints.
#   * Boot-time provider auth that notify reads from env: TELEGRAM_BOT_TOKEN,
#     DISCORD_BOT_TOKEN. Everything else (FCM/Firebase, Twilio, SignalWire,
#     Resend, CF email) is a per-tenant integration created through the
#     connectivity API and stored encrypted in SQLite — those secrets are made
#     available in the environment for operator/API use but are not consumed at
#     boot. The one file-type secret we materialize here is the Firebase
#     service-account JSON (mounted at FIREBASE_SERVICE_ACCOUNT_FILE) which we
#     base64-encode into FIREBASE_SERVICE_ACCOUNT_B64.
#
# This script never prints a secret value. It is safe to run with the shell
# trace flag for debugging ONLY if you accept that exported names are logged;
# values are not echoed.

set -euo pipefail

log() { printf 'notify-prelude: %s\n' "$*" >&2; }

# --- 1. Engine (in-pod dagnats sidecar) defaults ------------------------------
# These match cmd/notify/main.go's envOr() defaults so the container behaves the
# same whether or not the orchestrator set them.
export NOTIFY_NATS_URL="${NOTIFY_NATS_URL:-nats://127.0.0.1:4222}"
export NOTIFY_ADDR="${NOTIFY_ADDR:-0.0.0.0:3000}"
export NOTIFY_DB="${NOTIFY_DB:-/data/notify.db}"

# --- 2. Mesh (consolidated onto the local dagnats hub) ------------------------
# In the consolidated topology, the local dagnats IS the sesh agents' hub: it
# leafs into the secured root, so engine NATS and mesh share one endpoint.
# Empty (or engine-equal) NOTIFY_MESH_NATS_URL is now the INTENDED config —
# notify reuses -nats-url for mesh dispatch and cross-machine routing happens
# at the NATS leaf layer. Only flag the case where mesh points somewhere else.
if [[ -z "${NOTIFY_MESH_NATS_URL:-}" || "${NOTIFY_MESH_NATS_URL}" == "${NOTIFY_NATS_URL}" ]]; then
  log "mesh and engine share the local dagnats hub (${NOTIFY_NATS_URL}) — expected; agents reach it via the root leaf."
else
  log "mesh NATS overridden to a separate endpoint (${NOTIFY_MESH_NATS_URL}); agents are expected off the local dagnats hub."
fi

# --- 3. Master key sanity (validate, do not print) ----------------------------
# Empty derives a fixed dev key (local only); a real prod deploy must set a
# 32-byte hex value. Length-check here so a malformed key fails fast at the
# entrypoint instead of deep inside the app.
if [[ -n "${NOTIFY_MASTER_KEY_HEX:-}" ]]; then
  key_len="${#NOTIFY_MASTER_KEY_HEX}"
  if [[ ! "${NOTIFY_MASTER_KEY_HEX}" =~ ^[0-9a-fA-F]+$ ]] || [[ "${key_len}" -ne 64 ]]; then
    log "ERROR: NOTIFY_MASTER_KEY_HEX must be 64 hex chars (32 bytes); got length ${key_len}."
    exit 1
  fi
  log "master key: set (encryption-at-rest active)."
else
  log "WARNING: NOTIFY_MASTER_KEY_HEX is empty; using the fixed DEV master key. Do NOT run production data through this."
fi

# --- 4. Firebase service-account file -> *_B64 --------------------------------
# If a Firebase service-account JSON is mounted (scion file-type secret or a
# bind mount), encode it into the env var the app/operator consumes. We never
# print the contents.
FIREBASE_SERVICE_ACCOUNT_FILE="${FIREBASE_SERVICE_ACCOUNT_FILE:-/run/secrets/firebase-service-account.json}"
if [[ -z "${FIREBASE_SERVICE_ACCOUNT_B64:-}" && -f "${FIREBASE_SERVICE_ACCOUNT_FILE}" ]]; then
  # base64 with no line wraps; busybox base64 (alpine) has no -w, so strip
  # newlines with tr for portability across coreutils/busybox.
  FIREBASE_SERVICE_ACCOUNT_B64="$(base64 < "${FIREBASE_SERVICE_ACCOUNT_FILE}" | tr -d '\n')"
  export FIREBASE_SERVICE_ACCOUNT_B64
  log "firebase: service-account JSON loaded from file -> FIREBASE_SERVICE_ACCOUNT_B64."
fi

# --- 5. Provider configuration summary (names only) ---------------------------
# Report which channels are wired so an operator can confirm the secret
# injection worked, without ever leaking a value.
configured=()
[[ -n "${TELEGRAM_BOT_TOKEN:-}" ]]            && configured+=("telegram")
[[ -n "${TELEGRAM_CHAT_ID:-}" ]]              && configured+=("telegram-chat-id")
[[ -n "${DISCORD_BOT_TOKEN:-}" ]]             && configured+=("discord")
[[ -n "${RESEND_API_KEY:-}" ]]                && configured+=("resend")
[[ -n "${TWILIO_ACCOUNT_SID:-}" ]]            && configured+=("twilio")
[[ -n "${SIGNALWIRE_PROJECT_ID:-}" || -n "${SIGNALWIRE_API_TOKEN:-}" ]] && configured+=("signalwire")
[[ -n "${FIREBASE_SERVICE_ACCOUNT_B64:-}" ]]  && configured+=("firebase/fcm")
[[ -n "${CF_API_TOKEN:-}" ]]                  && configured+=("cloudflare-email")
[[ -n "${NOTIFY_INBOUND_SECRET:-}" ]]         && configured+=("inbound-email")

if [[ ${#configured[@]} -gt 0 ]]; then
  log "providers configured: ${configured[*]}"
else
  log "no provider credentials detected in env (tenants can still add integrations via the connectivity API)."
fi

# Chat default scope: cold inbound drops silently if the default agent is unset.
if [[ -n "${TELEGRAM_BOT_TOKEN:-}${DISCORD_BOT_TOKEN:-}" && -z "${NOTIFY_CHAT_DEFAULT_AGENT:-}" ]]; then
  log "WARNING: a chat bot token is set but NOTIFY_CHAT_DEFAULT_AGENT is empty; cold inbound messages will be dropped."
fi

log "public base URL: ${NOTIFY_PUBLIC_BASE_URL:-<unset>}; addr: ${NOTIFY_ADDR}; db: ${NOTIFY_DB}"

# --- 6. Hand off to notify ----------------------------------------------------
log "exec notify serve"
exec /usr/local/bin/notify serve
