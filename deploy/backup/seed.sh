#!/usr/bin/env bash
# Load SYNTHETIC data into a THROWAWAY drill cluster through the platform's own services and HTTP handlers
# (internal/httpserver/drillseed_integration_test.go, build tags `integration drillseed`). Not part of any normal
# test run and refused for anything but a drill cluster port.
#
#   deploy/backup/seed.sh --drill-root /tmp/pgdrill-XXXXXX --port 5499 [--report FILE]
#   parameters: DRILL_SEED_TENANTS, DRILL_SEED_PLAYERS (per tenant), DRILL_SEED_DEPOSITS, DRILL_SEED_ROUNDS
#               (casino wager/win rounds per player), DRILL_SEED_WORKERS
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"

ROOT=""; PORT=""; REPORT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --drill-root) shift; ROOT="${1:-}" ;;
    --port) shift; PORT="${1:-}" ;;
    --report) shift; REPORT="${1:-}" ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done
case "$ROOT" in /tmp/pgdrill-??????) ;; *) bk_die "--drill-root must be a /tmp/pgdrill-XXXXXX directory created by the drill" ;; esac
bk_guard_target 127.0.0.1 "$PORT"
PASSFILE="${ROOT}/secrets/pgpass"
[ -f "$PASSFILE" ] || bk_die "no ${PASSFILE}"
OWNER_PW="$(awk -F: -v p="$PORT" '$1=="127.0.0.1" && $2==p && $4=="igaming" {print $5}' "$PASSFILE")"
[ -n "$OWNER_PW" ] || bk_die "no owner credential for port ${PORT} in the drill passfile"

cd "$REPO_ROOT"
TEST_DATABASE_URL="postgres://igaming:${OWNER_PW}@127.0.0.1:${PORT}/igaming_platform_dev?sslmode=disable" \
DRILL_SEED_ALLOW=throwaway-cluster \
DRILL_SEED_REPORT="${REPORT:-/dev/null}" \
  go test -tags 'integration drillseed' -run '^TestDrillSeedWorld$' -count=1 -timeout 3h ./internal/httpserver
