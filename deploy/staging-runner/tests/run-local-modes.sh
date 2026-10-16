#!/usr/bin/env bash
# End-to-end LOCAL test of every staging-runner mode against a THROWAWAY PostgreSQL 16 cluster (drill ports 5497-5499 under /tmp/pgdrill-*, never
# the shared 5432 cluster; see local-harness.sh). Asserts the RESULT records each mode must produce and the refusals it must make. Touches no AWS.
#
#   deploy/staging-runner/tests/run-local-modes.sh            full run (loads a small synthetic world through the platform's own services first:
#                                                              compiles internal/httpserver's drill seeder, a few minutes)
#   SEED_WORLD=0 deploy/staging-runner/tests/run-local-modes.sh   skip the world: the ledger is empty, so the two ledger-dependent verifier checks FAIL by design
#
# Needs: go, psql/pg_dump/pg_restore 16, openssl >= 3, age, jq, runuser (to run the throwaway server as the OS user postgres). Exit 0 = every assertion held.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
H="${HERE}/local-harness.sh"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"; bash "$H" down >/dev/null 2>&1 || true' EXIT
SEED_WORLD="${SEED_WORLD:-1}"

fails=0
ok() { echo "ok   $*"; }
bad() { echo "FAIL $*" >&2; fails=$((fails + 1)); }
# run <name> <expected exit> <mode> [K=V ...] -> output in $OUT/<name>.out
run() {
  local name="$1" want="$2"; shift 2
  local rc=0
  bash "$H" run "$@" >"${OUT}/${name}.out" 2>&1 || rc=$?
  [ "$rc" = "$want" ] && ok "${name}: exit ${rc}" || bad "${name}: exit ${rc}, expected ${want}"
}
has() { grep -qE "$2" "${OUT}/$1.out" && ok "$1: $3" || bad "$1: expected $3 (/$2/)"; }
hasnt() { if grep -qE "$2" "${OUT}/$1.out"; then bad "$1: must not contain $3 (/$2/)"; else ok "$1: no $3"; fi; }
gen() { head -c 300 /dev/urandom | tr -dc 'A-Za-z0-9' | head -c 64; }
sq() { bash "$H" psql -d igaming_platform_dev -c "$1" >/dev/null; }

bash "$H" up
# shellcheck source=/dev/null
. "${RUNNER_HARNESS_STATE:-/tmp/staging-runner-harness.state}"

if [ "$SEED_WORLD" = 1 ]; then
  export DRILL_SEED_TENANTS=2 DRILL_SEED_PLAYERS=3 DRILL_SEED_DEPOSITS=2 DRILL_SEED_ROUNDS=12 DRILL_SEED_WORKERS=2
  bash "${HERE}/../../backup/seed.sh" --drill-root "$HARNESS_ROOT" --port "$HARNESS_PORT" --report "${OUT}/seed.json" >"${OUT}/seed.log" 2>&1 || { tail -20 "${OUT}/seed.log" >&2; exit 2; }
fi

# ------------------------------------------------------------------ verify-db
run verify_db 0 verify-db
for n in role_owner_attributes role_runtime_attributes role_runtime_owns_nothing search_path_session_owner search_path_session_runtime secdef_functions_pin_search_path temp_revoked_runtime temp_revoked_public \
         runtime_cannot_write_schema_migrations tenant_tables_force_rls migration_head_equals_image database_environment_anchor runtime_session_identity migrate_verify; do
  has verify_db "^RESULT\\|${n}\\|PASS\\|" "$n PASS"
done
has verify_db '^SNAPSHOT\|grants\|igaming_runtime\|tables=[0-9]+\|columns=[0-9]+\|routines=[0-9]+\|sha256=[0-9a-f]{64}' "the grant snapshot digest"
cp "${OUT}/verify_db.out" "${OUT}/verify_db_first.out"
run verify_db_again 0 verify-db
[ "$(grep '^SNAPSHOT|grants|igaming_runtime|' "${OUT}/verify_db_first.out")" = "$(grep '^SNAPSHOT|grants|igaming_runtime|' "${OUT}/verify_db_again.out")" ] && ok "the grant snapshot is deterministic" || bad "the grant snapshot differs between two runs"
sq "GRANT INSERT ON schema_migrations TO igaming_runtime"
run verify_db_widened 1 verify-db
has verify_db_widened '^RESULT\|runtime_cannot_write_schema_migrations\|FAIL\|' "a widened schema_migrations grant is caught"
[ "$(grep '^SNAPSHOT|grants|igaming_runtime|' "${OUT}/verify_db_widened.out")" != "$(grep '^SNAPSHOT|grants|igaming_runtime|' "${OUT}/verify_db_first.out")" ] && ok "the grant snapshot digest changes with the widened grant" || bad "the snapshot digest did not change"
sq "REVOKE INSERT ON schema_migrations FROM igaming_runtime"
sq "ALTER TABLE brands NO FORCE ROW LEVEL SECURITY"
run verify_db_norls 1 verify-db
has verify_db_norls '^RESULT\|tenant_tables_force_rls\|FAIL\|.*lacking FORCE RLS=1' "a tenant table without FORCE RLS is caught"
sq "ALTER TABLE brands FORCE ROW LEVEL SECURITY"
HARNESS_APP_ENV=production run production_refused2 2 verify-db
has production_refused2 '^RUNNER\|REFUSED\|APP_ENV=production' "APP_ENV=production is refused"
has production_refused2 '^RUNNER\|END\|exit=2' "a refusal still ends with RUNNER|END"

# ------------------------------------------------------------------ seed-actor-proof
K1="stg-v1:$(gen)"
ACTOR_PROOF_KEYS="$K1" run actor_first 0 seed-actor-proof
has actor_first '^RESULT\|actor_proof_row\|PASS\|kid stg-v1 inserted' "the row is inserted"
has actor_first '^ACTOR\|row\|kid=stg-v1 status=active octets=48' "kid, status and octet length are reported, no key material"
ACTOR_PROOF_KEYS="$K1" run actor_again 0 seed-actor-proof
has actor_again 'already present with the identical secret' "idempotent"
ACTOR_PROOF_KEYS="stg-v1:$(gen)" run actor_conflict 1 seed-actor-proof
has actor_conflict 'DIFFERENT secret; refusing to overwrite' "a different secret for the same kid is refused"
ACTOR_PROOF_KEYS="stg-v9:$(gen)" run actor_wrongkid 3 seed-actor-proof
ACTOR_PROOF_KEYS="stg-v1:QUJD" run actor_short 3 seed-actor-proof
HARNESS_APP_ENV=production ACTOR_PROOF_KEYS="$K1" run actor_prod 2 seed-actor-proof
for f in actor_first actor_again actor_conflict actor_wrongkid actor_short; do hasnt "$f" "${K1#stg-v1:}" "the secret"; done
sq "ALTER ROLE igaming SET log_statement = 'all'"
ACTOR_PROOF_KEYS="$K1" run actor_logged 3 seed-actor-proof
has actor_logged 'log_statement=all: the secret would be written to the database log' "statement logging blocks the write"
sq "ALTER ROLE igaming RESET log_statement"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT count(*) FROM actor_proof_keys WHERE kid = 'stg-v1'")" = 1 ] && ok "exactly one stg-v1 actor_proof_keys row" || bad "unexpected stg-v1 actor_proof_keys rows"

# ------------------------------------------------------------------ backup-restore-drill, probes, pitr-verify
run drill 0 backup-restore-drill
has drill 'RESULT\|backup_set\|PASS\|manifest encryption=age, signed' "the backup set is age-encrypted and signed"
has drill 'RESULT\|restore_load\|PASS\|' "the two-phase restore loaded as a non-superuser owner login"
has drill 'RESULT\|verify_restored\|PASS\|VERIFY RESULT: PASS' "verify.sh passed on the restored database"
has drill 'RESULT\|restore_check_database_dropped\|PASS\|' "the restore-check database was dropped"
[ "$(grep -c '^RESULT|probe_write_.*|PASS|' "${OUT}/drill.out")" = 15 ] && ok "15 write/escalation probes denied" || bad "expected 15 denied write probes"
[ "$(grep -c '^RESULT|probe_abuse_.*|PASS|' "${OUT}/drill.out")" = 8 ] && ok "8 abuse probes denied" || bad "expected 8 denied abuse probes"
has drill '^PITR\|T_utc=[0-9T:.Z-]+' "T is printed"
has drill '^PITR\|expected_digests=TABLE=[0-9a-f]{16};' "the baseline digests are printed"
hasnt drill "BEGIN"" PRIVATE"" KEY" "private key text"
DIG="$(sed -n 's/^PITR|expected_digests=//p' "${OUT}/drill.out" | head -1)"
run probes 0 probe-backup-role
has probes 'SUMMARY\|probe-backup-role\|pass=24\|fail=0' "24 probe results (attributes + 15 + 8)"
sq "GRANT EXECUTE ON FUNCTION pg_advisory_lock(bigint) TO PUBLIC"
run probes_unhardened 1 probe-backup-role EXPECT_HARDENING=applied
has probes_unhardened 'probe_abuse_pg_advisory_lock\|FAIL' "an un-hardened primitive is a FAIL when hardening is expected"
run probes_refused 0 probe-backup-role EXPECT_HARDENING=refused
has probes_refused 'probe_abuse_pg_advisory_lock\|FINDING\|NOT denied: RESIDUAL' "...and a recorded residual FINDING when RDS refused the hardening"
sq "REVOKE EXECUTE ON FUNCTION pg_advisory_lock(bigint) FROM PUBLIC"

bash "$H" clone >/dev/null
# shellcheck source=/dev/null
. "${RUNNER_HARNESS_STATE:-/tmp/staging-runner-harness.state}"
run pitr 0 pitr-verify PITR_ENDPOINT=127.0.0.1 "PITR_PORT=${HARNESS_CLONE_PORT}" "EXPECTED_DIGESTS=${DIG}" EXPECT_ANCHOR=unrecorded
has pitr 'pitr_digest_TABLE\|PASS\|' "the clone's table digest equals the source at T"
BAD="$(printf '%s' "$DIG" | sed 's/TABLE=[0-9a-f]\{16\}/TABLE=0000000000000000/')"
run pitr_tampered 1 pitr-verify PITR_ENDPOINT=127.0.0.1 "PITR_PORT=${HARNESS_CLONE_PORT}" "EXPECTED_DIGESTS=${BAD}" EXPECT_ANCHOR=unrecorded
has pitr_tampered 'pitr_digest_TABLE\|FAIL\|' "a differing digest is a FAIL"
run pitr_primary 2 pitr-verify PITR_ENDPOINT=127.0.0.1 "PITR_PORT=${HARNESS_PORT}" "EXPECTED_DIGESTS=${DIG}"
run pitr_badhost 2 pitr-verify PITR_ENDPOINT=evil.example.com "EXPECTED_DIGESTS=${DIG}"

# the RDS-like BLOCKED outcome: the owner role is an rds_superuser member (as the RDS master is)
sq "CREATE ROLE rds_superuser NOLOGIN"
sq "GRANT rds_superuser TO igaming"
run drill_blocked 0 backup-restore-drill
has drill_blocked 'RESULT\|restore_provision\|BLOCKED\|.*holds' "restore.sh's refusal of a privileged owner role is recorded as BLOCKED with the exact error"
has drill_blocked 'RESULT\|source_verify\|INFO\|' "verify.sh still ran against the source with the backup baseline"
sq "REVOKE rds_superuser FROM igaming"
sq "DROP ROLE rds_superuser"

# ------------------------------------------------------------------ seed-market (last: it adds a ledger-less tenant)
bash "$H" psql -d igaming_platform_dev -f "${HERE}/fixture.sql" >/dev/null
P=(TENANT_SLUG=demo-casino BRAND_SLUG=demo-brand COUNTRY=SV AUTH_REF=TICKET-42 OPERATOR=ops-alice REASON=staging-bootstrap)
if [ "$SEED_WORLD" = 1 ]; then
  # the seeded world has several platform admins: without ACTOR_ID the tool must refuse to guess who the actor is
  MOCK_GEOLOCATION_COUNTRY=SV run market_ambiguous 1 seed-market "${P[@]}"
  has market_ambiguous 'pass -actor-id explicitly' "an ambiguous actor is refused"
fi
P+=(ACTOR_ID=00000000-0000-0000-0000-0000000000f1)
MOCK_GEOLOCATION_COUNTRY=SV run market_dry 0 seed-market "${P[@]}" DRY_RUN=1
has market_dry 'ceiling\|would be created' "dry run writes nothing"
MOCK_GEOLOCATION_COUNTRY=SV run market 0 seed-market "${P[@]}"
has market 'resolves_registration\|permitted' "registration permitted"
has market 'resolves_sportsbook_wagering\|permitted' "sportsbook wagering permitted"
MOCK_GEOLOCATION_COUNTRY=SV run market_again 0 seed-market "${P[@]}"
has market_again 'ceiling\|already present' "idempotent"
MOCK_GEOLOCATION_COUNTRY=DE run market_country 1 seed-market "${P[@]}"
run market_noref 2 seed-market TENANT_SLUG=demo-casino BRAND_SLUG=demo-brand COUNTRY=SV OPERATOR=x REASON=y
HARNESS_APP_ENV=production run market_prod 2 seed-market "${P[@]}"

if [ "$fails" -ne 0 ]; then echo "${fails} assertion(s) failed; outputs kept in ${OUT}" >&2; trap 'bash "$H" down >/dev/null 2>&1 || true' EXIT; exit 1; fi
echo "all local runner mode assertions held"
