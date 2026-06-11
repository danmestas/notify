#!/usr/bin/env bash
# operator-smoke.sh — exercise the full operator journey over HTTP.
#
# Usage:
#   BASE=http://localhost:3000 KEY=<api-key> ./scripts/operator-smoke.sh
#
# Required env:
#   BASE   — notify server base URL (no trailing slash)
#   KEY    — API key (the bare key, Authorization header is "ApiKey <KEY>")
#
# Optional env — SMTP/email leg (skipped when absent):
#   RESEND_HOST   — SMTP host (e.g. smtp.resend.com)
#   RESEND_PORT   — SMTP port (default: 587)
#   RESEND_USER   — SMTP username (e.g. resend)
#   RESEND_KEY    — SMTP password / API key
#   RESEND_FROM   — sender address (e.g. notify@example.com)
#   TO_EMAIL      — recipient subscriber email
#
# Optional env — Cloudflare Email leg (skipped when absent):
#   CF_EMAIL_ACCOUNT_ID — Cloudflare account ID
#   CF_EMAIL_TOKEN      — Cloudflare Email API token (never echoed)
#   CF_EMAIL_FROM       — sender address (e.g. notify@example.com)
#   TO_EMAIL            — recipient subscriber email (shared with SMTP leg)
#
# Optional env — push/ntfy leg (skipped when absent):
#   NTFY_BASE     — ntfy server base URL (e.g. https://ntfy.sh)
#   NTFY_TOKEN    — ntfy access token (may be empty for public server)
#   NTFY_TOPIC    — ntfy topic
#
# Dependencies: curl, jq (jq builds JSON bodies safely — credentials may
# contain characters that would break naive string interpolation).
#
# The script exits non-zero if any executed check fails.
set -euo pipefail

: "${BASE:?BASE is required}"
: "${KEY:?KEY is required}"

if ! command -v jq >/dev/null 2>&1; then
  echo "error: jq is required but not installed (used to build JSON bodies safely)" >&2
  exit 1
fi

AUTH="ApiKey ${KEY}"

PASS=0
FAIL=0

_pass() { echo "  PASS  $1"; PASS=$((PASS+1)); }
_fail() { echo "  FAIL  $1  —  $2"; FAIL=$((FAIL+1)); }

_post() {
  local url="$1" body="$2"
  curl -s -o /tmp/notify-smoke-resp.json -w "%{http_code}" \
    -X POST "$url" \
    -H "Authorization: $AUTH" \
    -H "Content-Type: application/json" \
    -d "$body"
}

_put() {
  local url="$1" body="$2"
  curl -s -o /tmp/notify-smoke-resp.json -w "%{http_code}" \
    -X PUT "$url" \
    -H "Authorization: $AUTH" \
    -H "Content-Type: application/json" \
    -d "$body"
}

_get() {
  local url="$1"
  curl -s -o /tmp/notify-smoke-resp.json -w "%{http_code}" \
    -X GET "$url" \
    -H "Authorization: $AUTH"
}

_body() { cat /tmp/notify-smoke-resp.json; }

echo
echo "========================================="
echo "  notify operator smoke — $BASE"
echo "========================================="

# ── healthz ───────────────────────────────────────────────────────────────────
echo
echo "── healthz"
CODE=$(_get "$BASE/healthz")
if [ "$CODE" = "200" ]; then
  _pass "GET /healthz → 200"
else
  _fail "GET /healthz" "status=$CODE body=$(_body)"
fi

# ── bootstrap subscriber ──────────────────────────────────────────────────────
echo
echo "── subscriber bootstrap"
SUB_ID="smoke-sub-$(date +%s)"
SUBSCRIBER_BODY="{\"subscriberId\":\"$SUB_ID\",\"firstName\":\"Smoke\",\"email\":\"${TO_EMAIL:-smoke@example.com}\"}"
CODE=$(_post "$BASE/v1/subscribers" "$SUBSCRIBER_BODY")
if [ "$CODE" = "201" ]; then
  _pass "POST /v1/subscribers → 201"
else
  _fail "POST /v1/subscribers" "status=$CODE body=$(_body)"
fi

# ── in_app workflow (always runs) ─────────────────────────────────────────────
echo
echo "── in_app workflow"
INAPP_ID="smoke-inapp-$(date +%s)"
INAPP_WF_BODY="{\"name\":\"Smoke In-App\",\"triggerIdentifier\":\"$INAPP_ID\",\"steps\":[{\"stepId\":\"S1\",\"type\":\"in_app\",\"controlValues\":{\"subject\":\"Smoke OK\",\"body\":\"Smoke test passed\"}}]}"
CODE=$(_post "$BASE/v1/workflows" "$INAPP_WF_BODY")
if [ "$CODE" = "201" ]; then
  _pass "POST /v1/workflows (in_app) → 201"
  INAPP_OK=1
else
  _fail "POST /v1/workflows (in_app)" "status=$CODE body=$(_body)"
  INAPP_OK=0
fi

if [ "$INAPP_OK" = "1" ]; then
  TRIG_BODY="{\"name\":\"$INAPP_ID\",\"to\":{\"subscriberId\":\"$SUB_ID\"},\"payload\":{},\"transactionId\":\"smoke-inapp-tx-$(date +%s)\"}"
  CODE=$(_post "$BASE/v1/events/trigger" "$TRIG_BODY")
  if [ "$CODE" = "201" ]; then
    _pass "POST /v1/events/trigger (in_app) → 201"
  else
    _fail "POST /v1/events/trigger (in_app)" "status=$CODE body=$(_body)"
  fi
fi

# ── GET /v1/workflows ─────────────────────────────────────────────────────────
echo
echo "── list workflows"
CODE=$(_get "$BASE/v1/workflows")
if [ "$CODE" = "200" ]; then
  _pass "GET /v1/workflows → 200"
else
  _fail "GET /v1/workflows" "status=$CODE body=$(_body)"
fi

# ── invalid step type → 422 ───────────────────────────────────────────────────
echo
echo "── 422 validation"
INVALID_BODY="{\"name\":\"bad\",\"triggerIdentifier\":\"bad-$(date +%s)\",\"steps\":[{\"stepId\":\"X1\",\"type\":\"carrier_pigeon\",\"controlValues\":{}}]}"
CODE=$(_post "$BASE/v1/workflows" "$INVALID_BODY")
if [ "$CODE" = "422" ]; then
  _pass "POST /v1/workflows invalid step type → 422"
else
  _fail "POST /v1/workflows invalid step type" "status=$CODE (want 422) body=$(_body)"
fi

# ── email leg (conditional) ───────────────────────────────────────────────────
if [ -n "${RESEND_HOST:-}" ] && [ -n "${RESEND_KEY:-}" ] && [ -n "${RESEND_FROM:-}" ]; then
  echo
  echo "── email integration + workflow"
  EMAIL_IDENT="smoke-smtp-$(date +%s)"
  # jq -n --arg builds the body safely: credentials may contain '"' or '\'
  # that would break naive interpolation into a JSON string.
  SMTP_BODY=$(jq -n \
    --arg identifier "$EMAIL_IDENT" \
    --arg host "$RESEND_HOST" \
    --argjson port "${RESEND_PORT:-587}" \
    --arg username "${RESEND_USER:-resend}" \
    --arg password "$RESEND_KEY" \
    --arg from "$RESEND_FROM" \
    '{identifier: $identifier, host: $host, port: $port, username: $username, password: $password, from: $from, primary: true}')
  CODE=$(_post "$BASE/v1/integrations/email" "$SMTP_BODY")
  if [ "$CODE" = "201" ]; then
    RESP_BODY=$(_body)
    _pass "POST /v1/integrations/email → 201"
    if echo "$RESP_BODY" | grep -qF "$RESEND_KEY"; then
      _fail "password-not-echoed" "response contains the SMTP password"
    else
      _pass "password-not-echoed in response"
    fi
  else
    _fail "POST /v1/integrations/email" "status=$CODE body=$(_body)"
  fi

  if [ -n "${TO_EMAIL:-}" ]; then
    EMAIL_SUB_BODY=$(jq -n \
      --arg subscriberId "smoke-email-$(date +%s)" \
      --arg email "$TO_EMAIL" \
      '{subscriberId: $subscriberId, firstName: "EmailSmoke", email: $email}')
    CODE=$(_post "$BASE/v1/subscribers" "$EMAIL_SUB_BODY")
    EMAIL_SUB_ID=$(jq -r '.subscriberId' /tmp/notify-smoke-resp.json 2>/dev/null || echo "smoke-email-sub")

    EMAIL_WF_ID="smoke-email-wf-$(date +%s)"
    EMAIL_WF_BODY="{\"name\":\"Smoke Email\",\"triggerIdentifier\":\"$EMAIL_WF_ID\",\"steps\":[{\"stepId\":\"E1\",\"type\":\"email\",\"controlValues\":{\"subject\":\"Smoke email OK\",\"body\":\"This is a smoke test email.\"}}]}"
    CODE=$(_post "$BASE/v1/workflows" "$EMAIL_WF_BODY")
    if [ "$CODE" = "201" ]; then
      _pass "POST /v1/workflows (email) → 201"
      EMAIL_TRIG_BODY="{\"name\":\"$EMAIL_WF_ID\",\"to\":{\"subscriberId\":\"$EMAIL_SUB_ID\"},\"payload\":{},\"transactionId\":\"smoke-email-tx-$(date +%s)\"}"
      CODE=$(_post "$BASE/v1/events/trigger" "$EMAIL_TRIG_BODY")
      if [ "$CODE" = "201" ]; then
        _pass "POST /v1/events/trigger (email) → 201"
      else
        _fail "POST /v1/events/trigger (email)" "status=$CODE body=$(_body)"
      fi
    else
      _fail "POST /v1/workflows (email)" "status=$CODE body=$(_body)"
    fi
  fi
else
  echo "(skipping email leg — RESEND_HOST / RESEND_KEY / RESEND_FROM not set)"
fi

# ── cloudflare-email leg (conditional) ───────────────────────────────────────
if [ -n "${CF_EMAIL_ACCOUNT_ID:-}" ] && [ -n "${CF_EMAIL_TOKEN:-}" ] && [ -n "${CF_EMAIL_FROM:-}" ]; then
  echo
  echo "── cloudflare-email integration + workflow"
  CF_EMAIL_IDENT="smoke-cf-email-$(date +%s)"
  # jq -n --arg builds the body safely: credentials may contain special chars.
  # CF_EMAIL_TOKEN is passed via --arg and never interpolated into a bare string.
  CF_INT_BODY=$(jq -n \
    --arg identifier "$CF_EMAIL_IDENT" \
    --arg accountId "$CF_EMAIL_ACCOUNT_ID" \
    --arg apiToken "$CF_EMAIL_TOKEN" \
    --arg from "$CF_EMAIL_FROM" \
    '{identifier: $identifier, provider: "cloudflare-email", accountId: $accountId, apiToken: $apiToken, from: $from, primary: true}')
  CODE=$(_post "$BASE/v1/integrations/email" "$CF_INT_BODY")
  if [ "$CODE" = "201" ]; then
    RESP_BODY=$(_body)
    _pass "POST /v1/integrations/email (cloudflare-email) → 201"
    # Verify the API token is never echoed.
    if echo "$RESP_BODY" | grep -qF "$CF_EMAIL_TOKEN"; then
      _fail "cf-token-not-echoed" "response contains the Cloudflare Email API token"
    else
      _pass "cf-token-not-echoed in response"
    fi
    # Verify providerId is cloudflare-email.
    GOT_PROVIDER=$(echo "$RESP_BODY" | jq -r '.providerId' 2>/dev/null || echo "")
    if [ "$GOT_PROVIDER" = "cloudflare-email" ]; then
      _pass "providerId=cloudflare-email"
    else
      _fail "providerId" "got=$GOT_PROVIDER want=cloudflare-email"
    fi
  else
    _fail "POST /v1/integrations/email (cloudflare-email)" "status=$CODE body=$(_body)"
  fi

  if [ -n "${TO_EMAIL:-}" ]; then
    CF_SUB_BODY=$(jq -n \
      --arg subscriberId "smoke-cf-email-$(date +%s)" \
      --arg email "$TO_EMAIL" \
      '{subscriberId: $subscriberId, firstName: "CFEmailSmoke", email: $email}')
    CODE=$(_post "$BASE/v1/subscribers" "$CF_SUB_BODY")
    CF_SUB_ID=$(jq -r '.subscriberId' /tmp/notify-smoke-resp.json 2>/dev/null || echo "smoke-cf-sub")

    CF_WF_ID="smoke-cf-email-wf-$(date +%s)"
    CF_WF_BODY="{\"name\":\"Smoke CF Email\",\"triggerIdentifier\":\"$CF_WF_ID\",\"steps\":[{\"stepId\":\"CE1\",\"type\":\"email\",\"controlValues\":{\"subject\":\"CF Email smoke OK\",\"body\":\"This is a Cloudflare Email smoke test.\"}}]}"
    CODE=$(_post "$BASE/v1/workflows" "$CF_WF_BODY")
    if [ "$CODE" = "201" ]; then
      _pass "POST /v1/workflows (cloudflare-email) → 201"
      CF_TRIG_BODY="{\"name\":\"$CF_WF_ID\",\"to\":{\"subscriberId\":\"$CF_SUB_ID\"},\"payload\":{},\"transactionId\":\"smoke-cf-email-tx-$(date +%s)\"}"
      CODE=$(_post "$BASE/v1/events/trigger" "$CF_TRIG_BODY")
      if [ "$CODE" = "201" ]; then
        _pass "POST /v1/events/trigger (cloudflare-email) → 201"
      else
        _fail "POST /v1/events/trigger (cloudflare-email)" "status=$CODE body=$(_body)"
      fi
    else
      _fail "POST /v1/workflows (cloudflare-email)" "status=$CODE body=$(_body)"
    fi
  fi
else
  echo "(skipping cloudflare-email leg — CF_EMAIL_ACCOUNT_ID / CF_EMAIL_TOKEN / CF_EMAIL_FROM not set)"
fi

# ── push leg (conditional) ────────────────────────────────────────────────────
if [ -n "${NTFY_BASE:-}" ] && [ -n "${NTFY_TOPIC:-}" ]; then
  echo
  echo "── push integration + workflow"
  PUSH_IDENT="smoke-ntfy-$(date +%s)"
  PUSH_INT_BODY="{\"identifier\":\"$PUSH_IDENT\",\"baseUrl\":\"$NTFY_BASE\",\"token\":\"${NTFY_TOKEN:-}\",\"primary\":true}"
  CODE=$(_post "$BASE/v1/integrations/push" "$PUSH_INT_BODY")
  if [ "$CODE" = "201" ]; then
    _pass "POST /v1/integrations/push → 201"
  else
    _fail "POST /v1/integrations/push" "status=$CODE body=$(_body)"
  fi

  PUSH_SUB_ID="smoke-push-$(date +%s)"
  PUSH_SUB_BODY="{\"subscriberId\":\"$PUSH_SUB_ID\",\"firstName\":\"PushSmoke\"}"
  CODE=$(_post "$BASE/v1/subscribers" "$PUSH_SUB_BODY")

  CRED_BODY="{\"providerId\":\"ntfy\",\"channel\":\"push\",\"credentials\":{\"topic\":\"$NTFY_TOPIC\"}}"
  CODE=$(_put "$BASE/v1/subscribers/$PUSH_SUB_ID/credentials" "$CRED_BODY")
  if [ "$CODE" = "204" ]; then
    _pass "PUT /v1/subscribers/{id}/credentials → 204"
  else
    _fail "PUT /v1/subscribers/{id}/credentials" "status=$CODE body=$(_body)"
  fi

  PUSH_WF_ID="smoke-push-wf-$(date +%s)"
  PUSH_WF_BODY="{\"name\":\"Smoke Push\",\"triggerIdentifier\":\"$PUSH_WF_ID\",\"steps\":[{\"stepId\":\"P1\",\"type\":\"push\",\"controlValues\":{\"subject\":\"Smoke\",\"body\":\"push smoke OK\"}}]}"
  CODE=$(_post "$BASE/v1/workflows" "$PUSH_WF_BODY")
  if [ "$CODE" = "201" ]; then
    _pass "POST /v1/workflows (push) → 201"
    PUSH_TRIG_BODY="{\"name\":\"$PUSH_WF_ID\",\"to\":{\"subscriberId\":\"$PUSH_SUB_ID\"},\"payload\":{},\"transactionId\":\"smoke-push-tx-$(date +%s)\"}"
    CODE=$(_post "$BASE/v1/events/trigger" "$PUSH_TRIG_BODY")
    if [ "$CODE" = "201" ]; then
      _pass "POST /v1/events/trigger (push) → 201"
    else
      _fail "POST /v1/events/trigger (push)" "status=$CODE body=$(_body)"
    fi
  else
    _fail "POST /v1/workflows (push)" "status=$CODE body=$(_body)"
  fi
else
  echo "(skipping push leg — NTFY_BASE / NTFY_TOPIC not set)"
fi

# ── summary ───────────────────────────────────────────────────────────────────
echo
echo "========================================="
printf "  PASS: %d   FAIL: %d\n" "$PASS" "$FAIL"
echo "========================================="
echo

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
