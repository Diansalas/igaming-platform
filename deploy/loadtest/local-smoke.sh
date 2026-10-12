#!/usr/bin/env bash
# Local SMALL smoke run of platform-api (dev mode) on a PRIVATE scratch database, driven by
# deploy/loadtest/smoke. Plan and limits: docs/runbooks/load-and-soak-test-plan.md.
#
# This is a smoke test, NOT a load/soak/capacity test, and its numbers are NOT a capacity figure:
# one shared developer machine, one process, mock providers, synthetic data, modest concurrency.
#
#   deploy/loadtest/local-smoke.sh [--dry-run] [-c WORKERS] [-d DURATION]
#
# Required environment (existing CI/dev harness credentials, never defaulted or printed):
#   TEST_ADMIN_DATABASE_URL  igaming_test_admin (CREATEDB) - CREATE/DROP DATABASE only
#   TEST_DATABASE_URL        the migration-owner role; only user/password/host are used
# Optional: SMOKE_PORT (default 18080), DRILL_WORKDIR.
#
# Safety: local hosts only; the scratch database name is drill_bk_smoke_*; the server is stopped with
# `kill <its pid>` (never pkill/killall); no role or password is changed; nothing is sent anywhere.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
DRY_RUN=0
WORKERS=8
DURATION=10s
PORT="${SMOKE_PORT:-18080}"

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    -c) shift; WORKERS="${1:-}" ;;
    -d) shift; DURATION="${1:-}" ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
die() { echo "smoke: $*" >&2; exit 2; }
case "$WORKERS" in ''|*[!0-9]*) die "-c must be an integer" ;; esac
[ "$WORKERS" -ge 1 ] && [ "$WORKERS" -le 32 ] || die "-c must be between 1 and 32"
case "$PORT" in ''|*[!0-9]*) die "SMOKE_PORT must be an integer" ;; esac

: "${TEST_ADMIN_DATABASE_URL:?TEST_ADMIN_DATABASE_URL is required}"
: "${TEST_DATABASE_URL:?TEST_DATABASE_URL is required}"
for tool in psql go curl; do command -v "$tool" >/dev/null 2>&1 || die "required tool not found: $tool"; done

url_host() { local rest="${1#*://}"; rest="${rest#*@}"; rest="${rest%%/*}"; printf '%s' "${rest%%:*}"; }
url_with_db() {
  local base="$1" db="$2" rest="${1#*://}" query=""
  case "$base" in *\?*) query="?${base#*\?}" ;; esac
  printf '%s/%s%s' "${base%%://*}://${rest%%/*}" "$db" "$query"
}
for u in "$TEST_ADMIN_DATABASE_URL" "$TEST_DATABASE_URL"; do
  case "$(url_host "$u")" in 127.0.0.1|localhost) ;; *) die "refusing a non-local database host" ;; esac
done

DB="drill_bk_smoke_$(date +%s)_$$"
[[ "$DB" =~ ^drill_bk_smoke_[0-9_]+$ ]] || die "internal: unsafe database name"
DB_URL="$(url_with_db "$TEST_DATABASE_URL" "$DB")"
adm() { psql -X -q -At -v ON_ERROR_STOP=1 "$TEST_ADMIN_DATABASE_URL" "$@"; }

if [ "$DRY_RUN" -eq 1 ]; then
  echo "smoke: DRY RUN - would create ${DB}, migrate, seed, start platform-api on 127.0.0.1:${PORT} (APP_ENV=development), run ${WORKERS} workers for ${DURATION}, then stop and drop"
  adm -c "SELECT 'admin ok, createdb=' || rolcreatedb FROM pg_roles WHERE rolname = current_user"
  exit 0
fi

WORK="$(mktemp -d "${DRILL_WORKDIR:-${TMPDIR:-/tmp}}/smoke.XXXXXX")"
API_PID=""
cleanup() {
  local rc=$?
  set +e
  if [ -n "$API_PID" ]; then kill "$API_PID" 2>/dev/null; wait "$API_PID" 2>/dev/null; fi
  adm -c "DROP DATABASE IF EXISTS \"${DB}\" WITH (FORCE)" >/dev/null 2>&1
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT

( cd "$REPO_ROOT" && go build -o "${WORK}/migrate" ./cmd/migrate && go build -o "${WORK}/platform-api" ./cmd/platform-api )
adm -c "CREATE DATABASE \"${DB}\" OWNER igaming"
DATABASE_URL="$DB_URL" "${WORK}/migrate" -dir "${REPO_ROOT}/migrations" up >/dev/null
psql -X -q -v ON_ERROR_STOP=1 -v seed_deposits=2 -f "${REPO_ROOT}/deploy/drill/seed.sql" "$DB_URL" >/dev/null

# APP_ENV=development explicitly (an unset APP_ENV counts as production and refuses the mock adapters).
# AUTH_RATE_LIMIT_PER_MINUTE is raised so the per-IP limiter does not throttle the setup registrations;
# the limiter itself is therefore NOT exercised by this smoke.
APP_ENV=development HTTP_ADDR="127.0.0.1:${PORT}" OTEL_EXPORTER=none AUTH_RATE_LIMIT_PER_MINUTE=100000 \
  JWT_SIGNING_SECRET="smoke-local-not-a-secret-0123456789abcdef" DATABASE_URL="$DB_URL" \
  "${WORK}/platform-api" >"${WORK}/api.log" 2>&1 &
API_PID=$!

for _ in $(seq 1 60); do
  if curl -fsS -m 1 "http://127.0.0.1:${PORT}/readyz" >/dev/null 2>&1; then break; fi
  kill -0 "$API_PID" 2>/dev/null || { echo "smoke: platform-api exited early:" >&2; tail -5 "${WORK}/api.log" >&2; exit 1; }
  sleep 0.5
done
curl -fsS -m 2 "http://127.0.0.1:${PORT}/readyz" >/dev/null || { echo "smoke: platform-api never became ready" >&2; exit 1; }

( cd "$REPO_ROOT" && go run ./deploy/loadtest/smoke -url "http://127.0.0.1:${PORT}" -brand drill-brand-1 -c "$WORKERS" -d "$DURATION" -max-error-rate 0 )
echo "smoke: server log lines at ERROR level during the run (expected: the two alert_routing_not_ready lines and the payments sweeper 'no outbound credential resolver' line, all dev-mode facts):"
grep -c '"level":"ERROR"\|ERROR' "${WORK}/api.log" || true
