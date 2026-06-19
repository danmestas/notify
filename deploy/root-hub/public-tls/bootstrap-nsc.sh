#!/usr/bin/env bash
#
# bootstrap-nsc.sh — stand up the nsc trust chain for the root hub.
#
# Creates (idempotent-ish — re-running reuses existing entities):
#   * an OPERATOR            (the root of trust for the mesh)
#   * a SYS  account         (system account, wired as the server's system_account)
#   * a MESH account         (where the agents/notify traffic lives)
#   * a MINI-LEAF user       (the Mini dagnats leaf identity)
#
# Then exports the Mini leaf's `.creds` and prints:
#   * where to copy the .creds (→ the Mini's mini_leaf_creds file-type secret)
#   * the resolver / operator / system_account lines to paste into nats-server.conf
#
# It NEVER echoes secret material (seeds, private keys, JWT bodies) to stdout
# beyond the necessary file PATHS. The .creds file itself is written to disk with
# restrictive permissions; treat it as a credential.
#
# Requires: nsc (https://github.com/nats-io/nsc). If absent, the script detects
# this and prints install instructions instead of failing opaquely.

set -euo pipefail

# --- tunables (override via env) ---------------------------------------------
OPERATOR="${OPERATOR:-mesh-root}"
SYS_ACCOUNT="${SYS_ACCOUNT:-SYS}"
MESH_ACCOUNT="${MESH_ACCOUNT:-mesh}"
MINI_USER="${MINI_USER:-mini-leaf}"
# Where to drop the exported leaf creds and the generated resolver/server bits.
OUT_DIR="${OUT_DIR:-$(pwd)/nsc-out}"
CREDS_OUT="${CREDS_OUT:-${OUT_DIR}/mini-leaf.creds}"
# auth.conf is the REQUIRED include that makes nats-server.conf fail-closed. It
# must land NEXT TO nats-server.conf so `include "auth.conf"` resolves. Defaults
# to this script's own directory (deploy/root-hub/public-tls/). Override AUTH_OUT to write
# it elsewhere (e.g. /etc/nats/ on the deployed host).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTH_OUT="${AUTH_OUT:-${SCRIPT_DIR}/auth.conf}"

log() { printf 'bootstrap-nsc: %s\n' "$*" >&2; }

require_nsc() {
  if ! command -v nsc >/dev/null 2>&1; then
    cat >&2 <<'EOF'
bootstrap-nsc: nsc CLI not found on PATH.

Install it, then re-run:

  # macOS / Linux (Homebrew):
  brew install nats-io/nats-tools/nsc

  # or the official installer:
  curl -L https://raw.githubusercontent.com/nats-io/nsc/main/install.sh | sh

Docs: https://docs.nats.io/using-nats/nats-tools/nsc
EOF
    exit 1
  fi
}

# entity_exists KIND NAME — true if `nsc list KIND` already shows NAME.
# KIND is one of: operators | accounts | users (users needs an account ctx).
op_exists()   { nsc list operators 2>/dev/null | grep -qw "$1"; }
acct_exists() { nsc list accounts  2>/dev/null | grep -qw "$1"; }
user_exists() { nsc list users -a "$1" 2>/dev/null | grep -qw "$2"; }

main() {
  require_nsc
  mkdir -p "${OUT_DIR}"
  chmod 700 "${OUT_DIR}"

  # --- operator -------------------------------------------------------------
  if op_exists "${OPERATOR}"; then
    log "operator '${OPERATOR}' already exists; reusing."
    nsc env -o "${OPERATOR}" >/dev/null
  else
    # --sys provisions the SYS account alongside the operator. -- generate a
    # signing key so the operator's identity key can stay offline.
    log "creating operator '${OPERATOR}' (+ SYS account)."
    nsc add operator --name "${OPERATOR}" --sys --generate-signing-key
  fi

  # `nsc add operator --sys` already provisions the system account named "SYS"
  # and wires it as the operator's system_account. We must point system_account
  # at THAT account — never mint a second one, or the server would reference an
  # orphan account that the operator doesn't recognize as the system account.
  #
  # So: ignore any custom SYS_ACCOUNT override that doesn't already exist as a
  # real account. A non-"SYS" value here would create an unwired orphan; guard
  # against it and fall back to the operator's actual SYS.
  if [[ "${SYS_ACCOUNT}" != "SYS" ]] && ! acct_exists "${SYS_ACCOUNT}"; then
    log "WARNING: SYS_ACCOUNT='${SYS_ACCOUNT}' does not exist; the operator's"
    log "         '--sys' account is 'SYS'. Using 'SYS' to avoid an orphan"
    log "         system account. (Override only with a name nsc already knows.)"
    SYS_ACCOUNT="SYS"
  fi
  if ! acct_exists "${SYS_ACCOUNT}"; then
    # Defensive: an operator imported without --sys. Create + wire it explicitly.
    log "system account '${SYS_ACCOUNT}' missing; creating and wiring it."
    nsc add account --name "${SYS_ACCOUNT}"
    nsc edit operator --system-account "${SYS_ACCOUNT}"
  fi
  log "system account: ${SYS_ACCOUNT}"

  # --- mesh account ---------------------------------------------------------
  if acct_exists "${MESH_ACCOUNT}"; then
    log "account '${MESH_ACCOUNT}' already exists; reusing."
  else
    log "creating mesh account '${MESH_ACCOUNT}'."
    nsc add account --name "${MESH_ACCOUNT}"
  fi

  # --- Mini leaf user -------------------------------------------------------
  if user_exists "${MESH_ACCOUNT}" "${MINI_USER}"; then
    log "user '${MINI_USER}' already exists in '${MESH_ACCOUNT}'; reusing."
  else
    log "creating leaf user '${MINI_USER}' in account '${MESH_ACCOUNT}'."
    nsc add user --account "${MESH_ACCOUNT}" --name "${MINI_USER}"
  fi

  # --- export the Mini leaf .creds -----------------------------------------
  # nsc generate creds writes the JWT + nkey seed bundle the leaf authenticates
  # with. Restrictive perms; we print only the path.
  ( umask 177
    nsc generate creds --account "${MESH_ACCOUNT}" --name "${MINI_USER}" > "${CREDS_OUT}" )
  log "Mini leaf creds written: ${CREDS_OUT}"

  # --- auth.conf (the REQUIRED fail-closed include) ------------------------
  # nats-server.conf does `include "auth.conf"` and will NOT start without it.
  # `nsc generate config` emits the operator JWT + system_account + account
  # resolver — exactly the auth block nats-server needs. This is PUBLIC material
  # (JWTs + public keys + a resolver dir path), NOT a private seed, but we keep
  # it out of git anyway (.gitignore) and write it with restrictive perms.
  ( umask 077
    if ! nsc generate config --nats-resolver > "${AUTH_OUT}" 2>/dev/null; then
      nsc generate config --mem-resolver > "${AUTH_OUT}"
    fi )
  log "auth.conf written (fail-closed include): ${AUTH_OUT}"

  cat >&2 <<EOF

bootstrap-nsc: done.

Next steps (no secret values are printed — only paths):

  1. Wire the root server (auth.conf is the REQUIRED fail-closed include):
     - auth.conf was written next to nats-server.conf:
         ${AUTH_OUT}
       nats-server.conf already does \`include "auth.conf"\`, so no editing is
       needed — the auth block is now present. Without this file the server
       refuses to start (fail-closed: never an anonymous listener).
     - Start nats-server: nats-server -c nats-server.conf
     - Do NOT commit auth.conf (it is gitignored).

  2. Hand the Mini its leaf identity:
     - Copy ${CREDS_OUT}
       to the Mini and stage it as the file-type secret:
         scion hub secret set --type file \\
           --target /run/secrets/mini-leaf.creds mini_leaf_creds @${CREDS_OUT}
       (or pass it to deploy/stage-creds.sh via MINI_LEAF_CREDS_FILE, or mount it
        with MINI_LEAF_CREDS_HOST_FILE for raw docker compose).
     - Set SESH_ROOT_LEAF_URL=tls://<root-host>:7422 in the Mini's environment.

  3. Add another machine later: rerun with MINI_USER=<machine>-leaf to mint a
     fresh user + .creds in the same mesh account, then copy its .creds to that
     machine.

  Treat ${CREDS_OUT} as a secret. Do NOT commit it.
EOF
}

main "$@"
