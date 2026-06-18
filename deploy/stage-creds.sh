#!/usr/bin/env bash
#
# stage-creds.sh — push every notify secret into scion's hub secret store.
#
# Mirrors darken's scripts/stage-creds.sh: pull each secret from Doppler
# (project `notify`, config `dev`) into a temp file, then
# `scion hub secret set --type env --target <NAME> <NAME> @<file>`. The scion
# broker projects each as a container env var when notify is spawned under the
# scion template (deploy/scion-agent.yaml).
#
# Two secret shapes are staged:
#   * env-type — projected as a container env var of the same name (the bulk
#     of the list below, incl. SESH_ROOT_LEAF_URL, the root leaf listener URL).
#   * file-type — materialized to a path inside the container. The dagnats leaf
#     credentials (mini_leaf_creds → /run/secrets/mini-leaf.creds) are staged
#     this way because dagnats's DAGNATS_LEAF_CREDENTIALS wants a .creds file.
#
# Properties:
#   * Idempotent — re-running with the same Doppler state is a no-op upstream.
#   * Soft-fails per secret — a missing Doppler value skips that one secret;
#     the rest still stage. Partial environments stage what they have.
#   * Never echoes a secret value; temp files are 0600 and removed on exit.
#
# Usage:
#   deploy/stage-creds.sh                # all secrets (env + leaf creds file)
#   deploy/stage-creds.sh TELEGRAM_BOT_TOKEN DISCORD_BOT_TOKEN   # a subset
#
# Leaf credentials source (file-type secret): the nsc-issued Mini leaf .creds.
# Provide it via MINI_LEAF_CREDS_FILE (a local path to the .creds emitted by
# deploy/root-hub/bootstrap-nsc.sh) OR via Doppler key SESH_MINI_LEAF_CREDS
# (the full .creds PEM as a multiline secret). The file path wins if both are
# set. When neither is present the leaf creds are skipped (standalone boot).
#
# Requires: scion CLI on PATH (with an authenticated hub), doppler CLI logged in.

set -euo pipefail

# ---- editable secret list ----------------------------------------------------
# Every name here is pulled from Doppler and pushed as a scion env secret of the
# same name. Add/remove freely as notify's provider surface changes.
SECRETS=(
  # chat (read by `notify serve` at boot)
  TELEGRAM_BOT_TOKEN
  TELEGRAM_CHAT_ID
  DISCORD_BOT_TOKEN
  # email
  RESEND_API_KEY
  CF_EMAIL_FROM
  CF_API_TOKEN
  # sms / voice
  TWILIO_ACCOUNT_SID
  TWILIO_AUTH_TOKEN
  SIGNALWIRE_PROJECT_ID
  SIGNALWIRE_API_TOKEN
  SIGNALWIRE_SPACE_URL
  # push (firebase / fcm)
  FIREBASE_SERVICE_ACCOUNT_B64
  FIREBASE_PROJECT_ID
  # notify core
  NOTIFY_MASTER_KEY_HEX
  NOTIFY_INBOUND_SECRET
  # ingress
  TUNNEL_TOKEN
  # mesh: root hub leaf listener URL (e.g. tls://root.example:7422). The leaf
  # CREDS themselves are a file-type secret staged separately (see below).
  SESH_ROOT_LEAF_URL
)

# Target path inside the container for the dagnats leaf credentials file. Must
# match DAGNATS_LEAF_CREDENTIALS in deploy/compose.hub.yml / scion-agent.yaml.
MINI_LEAF_CREDS_TARGET="${MINI_LEAF_CREDS_TARGET:-/run/secrets/mini-leaf.creds}"
MINI_LEAF_CREDS_SECRET_NAME="${MINI_LEAF_CREDS_SECRET_NAME:-mini_leaf_creds}"
# Local path to an nsc-issued .creds file (from bootstrap-nsc.sh). Optional.
MINI_LEAF_CREDS_FILE="${MINI_LEAF_CREDS_FILE:-}"
# Doppler key holding the .creds PEM inline (multiline secret). Optional.
MINI_LEAF_CREDS_DOPPLER_KEY="${MINI_LEAF_CREDS_DOPPLER_KEY:-SESH_MINI_LEAF_CREDS}"

DOPPLER_PROJECT="${DOPPLER_PROJECT:-notify}"
DOPPLER_CONFIG="${DOPPLER_CONFIG:-dev}"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

scion_present() { command -v scion >/dev/null 2>&1; }
doppler_present() { command -v doppler >/dev/null 2>&1; }

# push_env_secret NAME VALUE — write VALUE to a 0600 temp file and stage it.
push_env_secret() {
  local name="$1" value="$2"
  local file="${TMP_DIR}/${name}"
  ( umask 177; printf '%s' "${value}" > "${file}" )
  # set -e is suppressed inside functions invoked from "|| true" contexts
  # (POSIX), so check the exit code explicitly rather than relying on it.
  if scion hub secret set --type env --target "${name}" "${name}" "@${file}" >/dev/null; then
    rm -f "${file}"
    echo "stage-creds: ${name} pushed (env)"
  else
    rm -f "${file}"
    echo "stage-creds: ${name} push FAILED" >&2
    return 1
  fi
}

# push_file_secret NAME TARGET SRC — stage SRC as a file-type secret that the
# scion broker materializes at TARGET inside the container. Mirrors darken's
# `scion hub secret set --type file --target` pattern.
push_file_secret() {
  local name="$1" target="$2" src="$3"
  # set -e is suppressed inside functions invoked from "|| true" contexts
  # (POSIX), so check the exit code explicitly rather than relying on it.
  if scion hub secret set --type file --target "${target}" "${name}" "@${src}" >/dev/null; then
    echo "stage-creds: ${name} pushed (file -> ${target})"
  else
    echo "stage-creds: ${name} push FAILED" >&2
    return 1
  fi
}

# stage_leaf_creds — stage the dagnats leaf .creds as a file-type secret. Source
# precedence: MINI_LEAF_CREDS_FILE (local path) > Doppler MINI_LEAF_CREDS_DOPPLER_KEY
# (inline PEM). Skips softly when neither is available.
stage_leaf_creds() {
  local file
  if [[ -n "${MINI_LEAF_CREDS_FILE}" ]]; then
    if [[ ! -f "${MINI_LEAF_CREDS_FILE}" ]]; then
      echo "stage-creds: WARNING — MINI_LEAF_CREDS_FILE=${MINI_LEAF_CREDS_FILE} not found; skipping leaf creds." >&2
      return 0
    fi
    push_file_secret "${MINI_LEAF_CREDS_SECRET_NAME}" "${MINI_LEAF_CREDS_TARGET}" "${MINI_LEAF_CREDS_FILE}" || true
    return 0
  fi

  local value
  if ! value="$(doppler secrets get "${MINI_LEAF_CREDS_DOPPLER_KEY}" --plain \
        --project "${DOPPLER_PROJECT}" --config "${DOPPLER_CONFIG}" 2>/dev/null)"; then
    echo "stage-creds: WARNING — no leaf creds (set MINI_LEAF_CREDS_FILE or Doppler ${MINI_LEAF_CREDS_DOPPLER_KEY}); dagnats stays standalone." >&2
    return 0
  fi
  if [[ -z "${value}" ]]; then
    echo "stage-creds: WARNING — Doppler ${MINI_LEAF_CREDS_DOPPLER_KEY} is empty; skipping leaf creds." >&2
    return 0
  fi
  file="${TMP_DIR}/${MINI_LEAF_CREDS_SECRET_NAME}.creds"
  ( umask 177; printf '%s' "${value}" > "${file}" )
  push_file_secret "${MINI_LEAF_CREDS_SECRET_NAME}" "${MINI_LEAF_CREDS_TARGET}" "${file}" || true
  rm -f "${file}"
}

# stage_one NAME — pull NAME from Doppler and push it; skip softly if absent.
stage_one() {
  local name="$1" value
  if ! value="$(doppler secrets get "${name}" --plain \
        --project "${DOPPLER_PROJECT}" --config "${DOPPLER_CONFIG}" 2>/dev/null)"; then
    echo "stage-creds: WARNING — ${name} not in Doppler ${DOPPLER_PROJECT}/${DOPPLER_CONFIG}; skipping." >&2
    return 0
  fi
  if [[ -z "${value}" ]]; then
    echo "stage-creds: WARNING — ${name} is empty in Doppler; skipping." >&2
    return 0
  fi
  push_env_secret "${name}" "${value}" || true
}

main() {
  if ! scion_present; then
    echo "stage-creds: scion CLI not on PATH; cannot stage secrets." >&2
    exit 1
  fi
  if ! doppler_present; then
    echo "stage-creds: doppler CLI not on PATH; cannot read secrets." >&2
    exit 1
  fi

  local targets=() do_leaf=0
  if [[ $# -gt 0 ]]; then
    # Explicit subset: stage the named env secrets, and the leaf creds file
    # only if its secret name was named explicitly.
    for arg in "$@"; do
      if [[ "${arg}" == "${MINI_LEAF_CREDS_SECRET_NAME}" ]]; then
        do_leaf=1
      else
        targets+=("${arg}")
      fi
    done
  else
    # Full stage: every env secret plus the leaf creds file.
    targets=("${SECRETS[@]}")
    do_leaf=1
  fi

  for name in "${targets[@]}"; do
    stage_one "${name}"
  done
  if [[ "${do_leaf}" -eq 1 ]]; then
    stage_leaf_creds
  fi

  echo "stage-creds: done (${DOPPLER_PROJECT}/${DOPPLER_CONFIG})."
}

main "$@"
