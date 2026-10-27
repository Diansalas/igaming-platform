#!/usr/bin/env bash
# Screening capacity harness driver (CAP-A). TEST TOOLING: private capa_* databases only, synthetic data, MOCK provider.
#
#   run.sh base                       build the migrated base database capa_base (once per commit)
#   run.sh seed <scenario>            clone capa_base -> capa_seed_<scenario>, load the scenario (owner path), vacuum/analyse
#   run.sh measure <mode> <scenario> [runs=3] [seed-scenario]   (seed-scenario defaults to the scenario file's "seed_scenario", else the scenario)
#                                      for each run: clone capa_seed_<seed-scenario|scenario> -> capa_<mode>_<scenario>, run the
#                                      mode (fn|growth|runpass|stale|retry|crash|race), write results/<scenario>.<mode>.runN.json, drop the clone
#   run.sh explain <scenario> <sqlfile>  clone, run an explain/*.sql file with psql, drop
#   run.sh drop-all                   drop every capa_* database this harness created
#
# Required environment (existing dev/CI harness variables, never defaulted, never printed):
#   TEST_ADMIN_DATABASE_URL  CREATEDB role - CREATE/DROP DATABASE only (the same use as scripts/ and local-smoke.sh)
#   DATABASE_URL             the migration-owner role (only user/password/host are used; the database name is replaced)
#   TEST_RUNTIME_DATABASE_URL the igaming_runtime login (RunPass runs as the runtime role, like production)
# Optional: CAPA_OUT (results dir, default deploy/loadtest/screening/results).
# No sudo, no role/password changes, no pkill. Postgres settings are the DEFAULTS of the host and are recorded by env-info.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
OUT="${CAPA_OUT:-$HERE/results}"
BIN="${CAPA_BIN:-/tmp/capa_bench}"
: "${TEST_ADMIN_DATABASE_URL:?}" "${DATABASE_URL:?}" "${TEST_RUNTIME_DATABASE_URL:?}"
owner_url() { echo "${DATABASE_URL%/*}/$1?sslmode=disable"; }
admin() { psql "$TEST_ADMIN_DATABASE_URL" -q -v ON_ERROR_STOP=1 "$@"; }
OWNER_ROLE="$(psql "$DATABASE_URL" -Atc 'select current_user')"
RT_ROLE="$(psql "$TEST_RUNTIME_DATABASE_URL" -Atc 'select current_user')"
mkdir -p "$OUT"

build() { (cd "$ROOT" && go build -tags loadtest -o "$BIN" ./deploy/loadtest/screening/); }

dbn() { echo "${1//-/_}"; }
clone() { # clone <src> <dst>
  admin -c "DROP DATABASE IF EXISTS $2 WITH (FORCE)" -c "CREATE DATABASE $2 TEMPLATE $1 OWNER $OWNER_ROLE STRATEGY FILE_COPY" >/dev/null
}
dropdb() { admin -c "DROP DATABASE IF EXISTS $1 WITH (FORCE)" >/dev/null; }

case "${1:-}" in
base)
  admin -c "DROP DATABASE IF EXISTS capa_base WITH (FORCE)" -c "CREATE DATABASE capa_base OWNER $OWNER_ROLE" >/dev/null
  psql "$(owner_url capa_base)" -q -v ON_ERROR_STOP=1 >/dev/null <<SQL
GRANT CONNECT ON DATABASE capa_base TO $RT_ROLE;
GRANT USAGE ON SCHEMA public TO $RT_ROLE;
ALTER DEFAULT PRIVILEGES FOR ROLE $OWNER_ROLE IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO $RT_ROLE;
ALTER DEFAULT PRIVILEGES FOR ROLE $OWNER_ROLE IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO $RT_ROLE;
SQL
  (cd "$ROOT" && DATABASE_URL="$(owner_url capa_base)" go run ./cmd/migrate up >/dev/null)
  psql "$(owner_url capa_base)" -q -v ON_ERROR_STOP=1 -c "REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM $RT_ROLE" \
    -c "REVOKE UPDATE, DELETE, TRUNCATE ON sportsbook_bet_settlements FROM $RT_ROLE" -c "REVOKE DELETE, TRUNCATE ON withdrawal_requests FROM $RT_ROLE" >/dev/null
  echo "built capa_base";;
seed)
  build; sc="$2"; sdb="capa_seed_$(dbn "$sc")"; clone capa_base "$sdb"
  "$BIN" seed -scenario "$HERE/scenarios/$sc.json" -db "$sdb" -out "$OUT/$sc.seed.json"
  echo "seeded $sdb";;
measure)
  build; mode="$2"; sc="$3"; runs="${4:-3}"
  seedsc="${5:-$(sed -n 's/.*"seed_scenario": *"\([^"]*\)".*/\1/p' "$HERE/scenarios/$sc.json" | head -1)}"; seedsc="${seedsc:-$sc}"
  for r in $(seq 1 "$runs"); do
    db="capa_${mode}_$(dbn "$sc")"; clone "capa_seed_$(dbn "$seedsc")" "$db"
    psql "$(owner_url "$db")" -qAt -c "select 'loadavg=' || '$(cut -d' ' -f1-3 /proc/loadavg)'" >/dev/null
    echo "[$(date -u +%H:%M:%S)] $mode $sc run $r (loadavg $(cut -d' ' -f1-3 /proc/loadavg))"
    "$BIN" "$mode" -scenario "$HERE/scenarios/$sc.json" -db "$db" -run "$r" -out "$OUT/$sc.$mode.run$r.json"
    dropdb "$db"
  done;;
explain)
  sc="$2"; f="$3"; db="capa_explain_$(dbn "$sc")"; clone "capa_seed_$(dbn "$sc")" "$db"
  (cd "$(dirname "$f")" && psql "$(owner_url "$db")" -v ON_ERROR_STOP=1 -X -f "$(basename "$f")") ; dropdb "$db";;
drop-all)
  for d in $(admin -Atc "select datname from pg_database where datname like 'capa\_%'"); do dropdb "$d"; echo "dropped $d"; done;;
*) sed -n 2,20p "$0"; exit 2;;
esac
