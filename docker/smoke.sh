#!/usr/bin/env bash
#
# docker/smoke.sh -- brings up the notify + dagnats stack via compose and
# exercises it through the containers:
#   1. both images build and the stack starts
#   2. notify reaches "healthy" (which means it connected to the dagnats
#      sidecar -- serve only serves /healthz after engine.New + eventbus.Connect)
#   3. GET /healthz answers 200 without auth (the health-probe target)
#   4. `notify bootstrap` provisions a tenant on the shared volume
#   5. an authenticated subscriber upsert round-trips through the API + SQLite
#
# Used by CI and locally. Tears the stack (and volumes) down on exit.
set -euo pipefail

cd "$(dirname "$0")/.."

# Bootstrap and serve must share one master key so the minted API key decrypts.
export NOTIFY_MASTER_KEY_HEX="${NOTIFY_MASTER_KEY_HEX:-$(openssl rand -hex 32)}"

# Host port for notify's published API. Overridable because 3000 is a popular
# port: on a machine already using it (a dev laptop, or a CI host running other
# services) compose cannot bind and the whole smoke fails for reasons unrelated
# to the code under test. compose.yml reads the same variable.
NOTIFY_HOST_PORT="${NOTIFY_HOST_PORT:-3000}"
export NOTIFY_HOST_PORT
base="http://localhost:${NOTIFY_HOST_PORT}"

cleanup() { docker compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "==> build + up"
docker compose up -d --build

echo "==> wait for notify to become healthy"
cid="$(docker compose ps -q notify)"
status=""
for _ in $(seq 1 60); do
  status="$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo unknown)"
  [ "$status" = "healthy" ] && break
  [ "$status" = "unhealthy" ] && break
  sleep 2
done
if [ "$status" != "healthy" ]; then
  echo "!! notify never became healthy (status=$status); logs:"
  docker compose logs --no-color | tail -80
  exit 1
fi

echo "==> GET /healthz (unauthenticated)"
curl -fsS "${base}/healthz" | grep -q '"status":"ok"'

echo "==> bootstrap a tenant (exec inside the running notify container)"
out="$(docker compose exec -T notify /usr/local/bin/notify bootstrap --org Acme --env dev)"
key="$(printf '%s' "$out" | grep -oE '"apiKey": *"[^"]+"' | sed -E 's/.*"apiKey": *"([^"]+)".*/\1/')"
if [ -z "$key" ]; then
  echo "!! no apiKey from bootstrap; output was:"; printf '%s\n' "$out"; exit 1
fi

echo "==> authenticated subscriber upsert through the container"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: ApiKey ${key}" -H 'Content-Type: application/json' \
  -d '{"subscriberId":"smoke_user","firstName":"Ada"}' \
  "${base}/v1/subscribers")"
if [ "$code" != "201" ]; then
  echo "!! subscriber upsert returned HTTP ${code}, want 201"; exit 1
fi

echo "SMOKE_OK"
