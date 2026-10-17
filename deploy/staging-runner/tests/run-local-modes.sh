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
gen() { head -c 3000 /dev/urandom | tr -dc 'A-Za-z0-9' | head -c 64; }
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
         runtime_cannot_write_schema_migrations tenant_tables_force_rls migration_head_equals_image database_environment_anchor leftover_restore_loader_role leftover_restore_check_database runtime_session_identity migrate_verify; do
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
run verify_db_require_anchor 1 verify-db REQUIRE_ANCHOR=1
has verify_db_require_anchor '^RESULT\|database_environment_anchor\|FAIL\|' "REQUIRE_ANCHOR=1: a blank anchor (= production) is a FAIL (SRP3-1)"

# ------------------------------------------------------------------ SRP3-6: the entrypoint cannot be made to trace
SUPERPW="$(cat "${HARNESS_ROOT}/secrets/super.pw")"
OWNERPW="$(awk -F: '$4=="igaming" {print $5; exit}' "${HARNESS_ROOT}/secrets/pgpass")"
RUNTIMEPW="$(awk -F: '$4=="igaming_runtime" {print $5; exit}' "${HARNESS_ROOT}/secrets/pgpass")"
# through the image's ENTRYPOINT (boot.sh: env -i allow-list): SHELLOPTS / BASH_ENV / PS4 / BASH_XTRACEFD in the container environment are dropped
echo 'echo "BASH_ENV_FILE_WAS_SOURCED"' >"${OUT}/bashenv.sh"
run trace_boot 0 verify-db SHELLOPTS=xtrace BASHOPTS=extdebug "BASH_ENV=${OUT}/bashenv.sh" "PS4=+TRACE " BASH_XTRACEFD=2 ENV="${OUT}/bashenv.sh"
has trace_boot '^RUNNER\|END\|exit=0' "the run with tracing variables in the environment still completes"
for sec in "$SUPERPW" "$OWNERPW" "$RUNTIMEPW"; do hasnt trace_boot "$sec" "a database password"; done
hasnt trace_boot '^\+TRACE|^\+ ' "an xtrace line"
hasnt trace_boot 'BASH_ENV_FILE_WAS_SOURCED' "the BASH_ENV file's output"
hasnt trace_boot ':\*:(igaming|postgres|igaming_runtime):' "a passfile line"
# the entrypoint run DIRECTLY (not through boot.sh), with the same variables: its own set +x comes before any secret is touched
HARNESS_ENTRY=entrypoint.sh run trace_direct 0 verify-db SHELLOPTS=xtrace "PS4=+TRACE "
for sec in "$SUPERPW" "$OWNERPW" "$RUNTIMEPW"; do hasnt trace_direct "$sec" "a database password (entrypoint run directly)"; done
hasnt trace_direct ':\*:(igaming|postgres|igaming_runtime):' "a passfile line (entrypoint run directly)"
has trace_direct '^RUNNER\|END\|exit=0' "the direct run completes"
# control: the SAME run against a copy of the entrypoint WITHOUT its `set +x +v` line must leak, which proves the assertions above are able to fail
CTL="${OUT}/ctl"
mkdir -p "${CTL}/deploy"
cp -r "${HERE}/.." "${CTL}/deploy/staging-runner"
ln -s "${HERE}/../../backup" "${CTL}/deploy/backup"
ln -s "${HERE}/../../drill" "${CTL}/deploy/drill"
cp -r "${HERE}/../../../migrations" "${CTL}/migrations"
sed -i '/^set +x +v$/d' "${CTL}/deploy/staging-runner/entrypoint.sh"
HARNESS_ENTRY="${CTL}/deploy/staging-runner/entrypoint.sh" run trace_control 0 verify-db SHELLOPTS=xtrace "PS4=+TRACE " "RN_ROOT=${CTL}"
has trace_control '^\+ RN_S_MASTER=' "control: without set +x the same SHELLOPTS=xtrace DOES trace the secret capture"
has trace_control "$SUPERPW" "control: ...and the master password then appears (so the 'no password' assertions are able to fail)"

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
sq "ALTER ROLE igaming SET log_transaction_sample_rate = 1"
ACTOR_PROOF_KEYS="$K1" run actor_txn_sample 3 seed-actor-proof
has actor_txn_sample 'log_transaction_sample_rate is not 0' "transaction sampling blocks the write (SRP3-5)"
sq "ALTER ROLE igaming RESET log_transaction_sample_rate"
sq "ALTER ROLE igaming SET log_statement_sample_rate = 1"
sq "ALTER ROLE igaming SET log_min_duration_sample = 0"
ACTOR_PROOF_KEYS="$K1" run actor_stmt_sample 3 seed-actor-proof
has actor_stmt_sample 'sampled statement logging' "statement sampling blocks the write (SRP3-5)"
sq "ALTER ROLE igaming RESET log_statement_sample_rate"
sq "ALTER ROLE igaming RESET log_min_duration_sample"
ACTOR_PROOF_KEYS="$K1" run actor_clean_again 0 seed-actor-proof
has actor_clean_again 'actor_proof_logging_safe\|PASS' "with the settings reset the write is allowed again"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT count(*) FROM actorproof_private.actor_proof_keys WHERE kid = 'stg-v1'")" = 1 ] && ok "exactly one stg-v1 actorproof_private.actor_proof_keys row" || bad "unexpected stg-v1 actor_proof_keys rows"

# ------------------------------------------------------------------ backup-restore-drill, probes, pitr-verify
run drill 0 backup-restore-drill
has drill 'RESULT\|backup_set\|PASS\|manifest encryption=age, signed' "the backup set is age-encrypted and signed"
has drill 'RESULT\|restore_load\|PASS\|' "the two-phase restore loaded as a non-superuser owner login"
has drill 'RESULT\|verify_restored\|PASS\|VERIFY RESULT: PASS' "verify.sh passed on the restored database"
has drill 'RESULT\|restore_check_database_dropped\|PASS\|' "the restore-check database was dropped"
[ "$(grep -c '^RESULT|probe_write_.*|PASS|' "${OUT}/drill.out")" = 15 ] && ok "15 write/escalation probes denied" || bad "expected 15 denied write probes"
[ "$(grep -c '^RESULT|probe_abuse_.*|PASS|' "${OUT}/drill.out")" = 9 ] && ok "9 abuse probes denied" || bad "expected 9 denied abuse probes"
[ "$(grep -c '^RESULT|probe_key_.*|PASS|' "${OUT}/drill.out")" = 3 ] && ok "3 actor-proof key probes denied (F1)" || bad "expected 3 denied actor-proof key probes"
has drill 'RESULT\|probe_pack_complete\|PASS\|all 27 probes ran' "the drill counted every probe"
has drill '^PITR\|T_utc=[0-9T:.Z-]+' "T is printed"
has drill '^PITR\|expected_digests=TABLE=[0-9a-f]{16};' "the baseline digests are printed"
hasnt drill "BEGIN"" PRIVATE"" KEY" "private key text"
DIG="$(sed -n 's/^PITR|expected_digests=//p' "${OUT}/drill.out" | head -1)"
run probes 0 probe-backup-role
has probes 'SUMMARY\|probe-backup-role\|pass=29\|fail=0' "29 passes (attributes + 15 + 9 + 3 + the completeness check)"
sq "GRANT EXECUTE ON FUNCTION pg_advisory_lock(bigint) TO PUBLIC"
run probes_unhardened 1 probe-backup-role EXPECT_HARDENING=applied
has probes_unhardened 'probe_abuse_pg_advisory_lock\|FAIL' "an un-hardened primitive is a FAIL when hardening is expected"
run probes_refused 0 probe-backup-role EXPECT_HARDENING=refused
has probes_refused 'probe_abuse_pg_advisory_lock\|FINDING\|NOT denied: RESIDUAL' "...and a recorded residual FINDING when RDS refused the hardening"
sq "REVOKE EXECUTE ON FUNCTION pg_advisory_lock(bigint) FROM PUBLIC"
# SRP3-2: fail closed. A write privilege granted to the backup role is a FAIL even though the statement fails later for a non-privilege reason
sq "GRANT INSERT ON tenants TO igaming_backup"
run probes_granted_insert 1 probe-backup-role
has probes_granted_insert 'probe_write_insert_tenants\|FAIL\|' "a granted INSERT is a FAIL (SQLSTATE is not a privilege error, or the insert succeeded)"
sq "REVOKE INSERT ON tenants FROM igaming_backup"
# F1: a backup role that could reach the actor-proof key (USAGE on the schema + SELECT on the table) FAILS the key probe
sq "GRANT USAGE ON SCHEMA actorproof_private TO igaming_backup"
sq "GRANT SELECT ON actorproof_private.actor_proof_keys TO igaming_backup"
run probes_key_granted 1 probe-backup-role
has probes_key_granted 'probe_key_read_actor_proof_secret\|FAIL\|NOT denied' "a backup role that can read the signing key is a FAIL"
sq "REVOKE SELECT ON actorproof_private.actor_proof_keys FROM igaming_backup"
sq "REVOKE USAGE ON SCHEMA actorproof_private FROM igaming_backup"
# a dead credential: nothing connects, so nothing may be reported as denied, and the run must not exit 0
echo "not-the-backup-password" >"${OUT}/wrong-backup.pw"
chmod 600 "${OUT}/wrong-backup.pw"
run probes_noconn 1 probe-backup-role "RUNNER_BACKUP_PASSWORD_FILE=${OUT}/wrong-backup.pw"
has probes_noconn 'probe_pack_complete\|FAIL\|only 0 of 27 probes ran' "a pack where no probe could connect FAILS (never a zero-PASS exit 0)"
hasnt probes_noconn 'probe_(write|abuse|key)_[a-z_]+\|PASS\|' "a PASS for a probe that never ran"
has probes_noconn 'SUMMARY\|probe-backup-role\|pass=0\|fail=[1-9]' "the summary shows pass=0 and a failure"

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
run pitr_notpitr 2 pitr-verify PITR_ENDPOINT=igaming-staging-db.abcdefgh1234.eu-central-1.rds.amazonaws.com "EXPECTED_DIGESTS=${DIG}"
has pitr_notpitr 'invalid format' "SRP3-10: an igaming-staging-* instance that is not a -db-pitr- clone is refused"

# the RDS-like BLOCKED outcome: the owner role is an rds_superuser member (as the RDS master is)
sq "CREATE ROLE rds_superuser NOLOGIN"
sq "GRANT rds_superuser TO igaming"
run drill_blocked 0 backup-restore-drill
has drill_blocked 'RESULT\|restore_provision\|BLOCKED\|.*holds' "restore.sh's refusal of a privileged owner role is recorded as BLOCKED with the exact error"
has drill_blocked 'RESULT\|source_verify\|INFO\|' "verify.sh still ran against the source with the backup baseline"
sq "REVOKE rds_superuser FROM igaming"
sq "DROP ROLE rds_superuser"

# ------------------------------------------------------------------ SRP3-9: leftovers of an interrupted drill are detected and removed
run verify_db_clean 0 verify-db
has verify_db_clean '^RESULT\|leftover_restore_loader_role\|PASS\|' "after the drills no restore-loader role remains"
has verify_db_clean '^RESULT\|leftover_restore_check_database\|PASS\|' "after the drills no restore-check database remains"
sq "CREATE ROLE igaming_restore_loader NOLOGIN"
sq "CREATE DATABASE igaming_restore_check"
sq "CREATE DATABASE igaming_restore_custom"
run verify_db_leftovers 1 verify-db
has verify_db_leftovers '^RESULT\|leftover_restore_loader_role\|FAIL\|' "a leftover loader role is a FAIL"
has verify_db_leftovers '^RESULT\|leftover_restore_check_database\|FAIL\|.*igaming_restore_check' "a leftover restore-check database is a FAIL and is named"
run cleanup_leftovers 0 cleanup CLEANUP_DB=igaming_restore_custom
has cleanup_leftovers 'cleanup_database_dropped\|PASS\|dropped leftover database igaming_restore_check' "cleanup drops the restore-check database"
has cleanup_leftovers 'cleanup_database_dropped\|PASS\|dropped leftover database igaming_restore_custom' "...and the custom-named one"
has cleanup_leftovers 'cleanup_loader_role\|PASS\|dropped leftover role igaming_restore_loader' "...and the loader role"
run cleanup_again 0 cleanup
has cleanup_again 'cleanup_loader_role\|PASS\|no igaming_restore_loader role' "cleanup is idempotent"
run verify_db_after_cleanup 0 verify-db
# SRP3R-2: on RDS (forced here with RUNNER_LOCAL_IAM_ONLY=1) igaming_backup must be IAM-only; cleanup restores that state instead of making a password permanent
sq "CREATE ROLE rds_iam NOLOGIN"
run verify_db_iam_bad 1 verify-db RUNNER_LOCAL_IAM_ONLY=1
has verify_db_iam_bad '^RESULT\|backup_role_iam_member\|FAIL\|' "a backup role that is not an rds_iam member is a FAIL on RDS"
has verify_db_iam_bad '^RESULT\|leftover_backup_credential\|FAIL\|' "...and its finite validity is a FAIL"
run cleanup_iam 0 cleanup RUNNER_LOCAL_IAM_ONLY=1
has cleanup_iam 'cleanup_backup_credential\|PASS\|.*restored to IAM-only: PASSWORD NULL, VALID UNTIL infinity, member of rds_iam' "cleanup restores PASSWORD NULL + VALID UNTIL infinity + rds_iam"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT rolvaliduntil::text FROM pg_roles WHERE rolname = 'igaming_backup'")" = infinity ] && ok "the validity is infinity because the password is gone" || bad "unexpected validity"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT (rolpassword IS NULL)::text FROM pg_authid WHERE rolname = 'igaming_backup'")" = true ] && ok "igaming_backup has no password any more" || bad "a password remains on igaming_backup"
run verify_db_iam_ok 0 verify-db RUNNER_LOCAL_IAM_ONLY=1
has verify_db_iam_ok '^RESULT\|backup_role_iam_member\|PASS\|' "an IAM-only backup role passes"
sq "DROP ROLE rds_iam"
run cleanup_badname 2 cleanup CLEANUP_DB=postgres
sq "CREATE DATABASE unrelated_db"
run verify_db_unknown_db 1 verify-db
has verify_db_unknown_db 'leftover_restore_check_database\|FAIL\|.*unrelated_db' "an unexpected extra database is reported"
run cleanup_unknown_db 0 cleanup
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT count(*) FROM pg_database WHERE datname = 'unrelated_db'")" = 1 ] && ok "cleanup never drops a database it does not own" || bad "cleanup dropped an unrelated database"
sq "DROP DATABASE unrelated_db"

# ------------------------------------------------------------------ seed-market (last: it adds a ledger-less tenant)
bash "$H" psql -d igaming_platform_dev -f "${HERE}/fixture.sql" >/dev/null
P=(TENANT_SLUG=demo-casino BRAND_SLUG=demo-brand COUNTRY=SV AUTH_REF=TICKET-42 OPERATOR=ops-alice REASON=staging-bootstrap)
# ---- SRP3-1: no anchor row = production = every seeding command refuses
MOCK_GEOLOCATION_COUNTRY=SV run market_noanchor 1 seed-market "${P[@]}" ACTOR_ID=00000000-0000-0000-0000-0000000000f1
has market_noanchor 'no database_environment anchor' "seed-market refuses a database without an anchor row"
MOCK_GEOLOCATION_COUNTRY=SV run world_noanchor 1 seed-world TENANT_SLUG=acc-one BRAND_SLUG=acc-one-brand COUNTRY=SV AUTH_REF=TICKET-43 OPERATOR=ops-alice "REASON=staging acceptance world" ACTOR_ID=00000000-0000-0000-0000-0000000000f1 DRY_RUN=1 "SEED_WORLD_LOCAL_STORE=${OUT}/store.json"
has world_noanchor 'seed_world_plan\|FAIL\|.*no database_environment anchor' "seed-world refuses a database without an anchor row"
[ ! -e "${OUT}/store.json" ] && ok "nothing was uploaded" || bad "a store file exists after a refused run"
run anchor_production 2 record-anchor ANCHOR_VALUE=production AUTH_REF=TICKET-44 OPERATOR=ops-alice "REASON=plan 6.2"
run anchor_noreason 2 record-anchor ANCHOR_VALUE=staging AUTH_REF=TICKET-44 OPERATOR=ops-alice
# SRP3R-1: record-anchor writes only on a FRESH database. This one holds tenants (and, with the drill world, players and ledger entries): refused.
run anchor_refused_live 1 record-anchor ANCHOR_VALUE=staging AUTH_REF=TICKET-44 OPERATOR=ops-alice "REASON=plan 6.2 anchor"
has anchor_refused_live 'record_anchor\|FAIL\|.*only records the anchor on a FRESH database' "record-anchor refuses a database that holds data"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT count(*) FROM database_environment")" = 0 ] && ok "nothing was written by the refused run" || bad "an anchor row exists after the refused run"
# a fresh (migrated, empty) database accepts it, and then it is idempotent
bash "$H" psql -d postgres -c "CREATE DATABASE igaming_fresh OWNER igaming" >/dev/null
DATABASE_URL="postgres://igaming:${OWNERPW}@127.0.0.1:${HARNESS_PORT}/igaming_fresh?sslmode=disable" "${HARNESS_ROOT}/bin/migrate" -dir "${HERE}/../../../migrations" up >/dev/null 2>&1
run anchor_record 0 record-anchor DB_NAME=igaming_fresh ANCHOR_VALUE=staging AUTH_REF=TICKET-44 OPERATOR=ops-alice "REASON=plan 6.2 anchor"
has anchor_record 'record_anchor\|PASS\|database_environment recorded as staging' "record-anchor records staging on a fresh database"
has anchor_record '^ANCHOR\|row\|staging\|by=' "the stored row is reported"
run anchor_record_again 0 record-anchor DB_NAME=igaming_fresh ANCHOR_VALUE=staging AUTH_REF=TICKET-44 OPERATOR=ops-alice "REASON=plan 6.2 anchor"
has anchor_record_again 'already staging; nothing written' "record-anchor is idempotent on the fresh database"
# a fresh database that gets one tenant is no longer fresh (a second fresh database, anchor still unset)
bash "$H" psql -d postgres -c "CREATE DATABASE igaming_fresh2 OWNER igaming" >/dev/null
DATABASE_URL="postgres://igaming:${OWNERPW}@127.0.0.1:${HARNESS_PORT}/igaming_fresh2?sslmode=disable" "${HARNESS_ROOT}/bin/migrate" -dir "${HERE}/../../../migrations" up >/dev/null 2>&1
bash "$H" psql -d igaming_fresh2 -f "${HERE}/fixture.sql" >/dev/null
run anchor_refused_tenant 1 record-anchor DB_NAME=igaming_fresh2 ANCHOR_VALUE=staging AUTH_REF=TICKET-44 OPERATOR=ops-alice "REASON=plan 6.2 anchor"
has anchor_refused_tenant 'only records the anchor on a FRESH database' "one tenant is enough to refuse"
bash "$H" psql -d postgres -c "DROP DATABASE igaming_fresh WITH (FORCE)" -c "DROP DATABASE igaming_fresh2 WITH (FORCE)" >/dev/null
# the owner of THIS (non-fresh, test) database records the anchor deliberately, as the runbook describes
sq "INSERT INTO database_environment (environment) VALUES ('staging')"
run anchor_again 0 record-anchor ANCHOR_VALUE=staging AUTH_REF=TICKET-44 OPERATOR=ops-alice "REASON=plan 6.2 anchor"
has anchor_again 'already staging; nothing written' "record-anchor is idempotent"
run anchor_other 1 record-anchor ANCHOR_VALUE=test AUTH_REF=TICKET-44 OPERATOR=ops-alice "REASON=plan 6.2 anchor"
has anchor_other 'already recorded as .staging.; it can never be changed' "a different value is refused (write-once)"
run verify_db_anchor_ok 0 verify-db REQUIRE_ANCHOR=1
has verify_db_anchor_ok '^RESULT\|database_environment_anchor\|PASS\|.*environment=staging' "REQUIRE_ANCHOR=1 passes once staging is recorded"
if [ "$SEED_WORLD" = 1 ]; then
  # the seeded world has several platform admins: without ACTOR_ID the tool must refuse to guess who the actor is
  MOCK_GEOLOCATION_COUNTRY=SV run market_ambiguous 1 seed-market "${P[@]}"
  has market_ambiguous 'pass the actor id explicitly' "an ambiguous actor is refused"
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

# ------------------------------------------------------------------ seed-world (SA-1): ACTIVE tenants, brands, market rows, staff; credentials ONLY in the store
W=(TENANT_SLUG=acc-one BRAND_SLUG=acc-one-brand SECOND_TENANT_SLUG=acc-two SECOND_BRAND_SLUG=acc-two-brand COUNTRY=SV AUTH_REF=TICKET-43 OPERATOR=ops-alice "REASON=staging acceptance world" ACTOR_ID=00000000-0000-0000-0000-0000000000f1 "SEED_WORLD_LOCAL_STORE=${OUT}/store.json")
MOCK_GEOLOCATION_COUNTRY=SV run world_dry 0 seed-world "${W[@]}" DRY_RUN=1
has world_dry 'seed_world_plan\|PASS\|.*5 staff account' "dry run plans five accounts"
[ ! -e "${OUT}/store.json" ] && ok "a dry run uploads nothing" || bad "a dry run wrote the store"
# a store that cannot be written: STOP before anything is created
MOCK_GEOLOCATION_COUNTRY=SV run world_nostore 3 seed-world "${W[@]/SEED_WORLD_LOCAL_STORE=*/SEED_WORLD_LOCAL_STORE=${OUT}/no-such-dir/store.json}"
has world_nostore 'NOTHING was created' "an unwritable credential store stops the run before any account exists"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT count(*) FROM tenants WHERE slug IN ('acc-one','acc-two')")" = 0 ] && ok "no tenant was created by the refused run" || bad "a tenant exists after the refused run"
# SRP3R-7: two concurrent seed-world runs are serialized: a held advisory lock makes a second run STOP with nothing written
bash "$H" psql -d igaming_platform_dev -c "SELECT pg_advisory_lock(7461020001); SELECT pg_sleep(120)" >/dev/null 2>&1 &
LOCKPID=$!
until [ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND classid = 1 AND objid = 3166052705")" = 1 ]; do sleep 0.2; done
MOCK_GEOLOCATION_COUNTRY=SV run world_locked 3 seed-world "${W[@]}"
has world_locked 'another seed-world run is in progress' "a concurrent run is refused while the lock is held"
[ ! -e "${OUT}/store.json" ] && ok "the refused concurrent run uploaded nothing" || bad "the concurrent run wrote the store"
bash "$H" psql -d igaming_platform_dev -c "SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND granted AND classid = 1 AND objid = 3166052705" >/dev/null
wait "$LOCKPID" 2>/dev/null || true
MOCK_GEOLOCATION_COUNTRY=SV run world 0 seed-world "${W[@]}"
has world 'RESULT\|seed_world\|PASS\|' "seed-world succeeds"
has world 'RESULT\|seed_world_credentials_planned\|PASS\|5 generated one-time password' "passwords stored before the accounts were created"
has world 'RESULT\|seed_world_credentials_stored\|PASS\|5 account' "tokens merged afterwards"
[ "$(jq -r '.actors | length' "${OUT}/store.json")" = 5 ] && ok "five accounts in the store" || bad "the store does not hold five accounts"
[ "$(jq -r '[.actors[] | select((.password | length) > 20 and (.bootstrap_token | length) > 20 and (.email | endswith("@acc-one.acceptance.invalid")))] | length' "${OUT}/store.json")" = 5 ] && ok "every account has an e-mail, a password and a bootstrap token" || bad "incomplete account entries"
[ "$(stat -c %a "${OUT}/store.json")" = 600 ] && ok "the store file is 0600" || bad "the store file is not 0600"
while IFS= read -r secret; do hasnt world "$secret" "a generated credential"; done < <(jq -r '.actors[] | .password, .bootstrap_token' "${OUT}/store.json")
hasnt world '"password"|bootstrap_token' "credential JSON"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT string_agg(slug || ':' || status, ',' ORDER BY slug) FROM tenants WHERE slug IN ('acc-one','acc-two')")" = "acc-one:active,acc-two:active" ] && ok "both synthetic tenants are ACTIVE" || bad "tenants are not active"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT string_agg(role, ',' ORDER BY role) FROM staff_users WHERE tenant_id = (SELECT id FROM tenants WHERE slug = 'acc-one')")" = "compliance,finance,finance,risk_manager,tenant_admin" ] && ok "staff roles: compliance, finance x2, risk_manager, tenant_admin" || bad "unexpected staff roles"
[ "$(bash "$H" psql -d igaming_platform_dev -c "SELECT count(DISTINCT person_id) FROM staff_users WHERE tenant_id = (SELECT id FROM tenants WHERE slug = 'acc-one')")" = 5 ] && ok "five distinct Persons (four-eyes needs two distinct finance Persons)" || bad "staff share a Person"
STORE_SUM="$(sha256sum "${OUT}/store.json" | cut -d' ' -f1)"
MOCK_GEOLOCATION_COUNTRY=SV run world_again 0 seed-world "${W[@]}"
has world_again 'seed_world_plan\|PASS\|.*0 staff account' "a second run plans nothing"
[ "$(sha256sum "${OUT}/store.json" | cut -d' ' -f1)" = "$STORE_SUM" ] && ok "idempotent: the stored credentials are unchanged" || bad "a re-run changed the stored credentials"
MOCK_GEOLOCATION_COUNTRY=DE run world_country 1 seed-world "${W[@]}"
MOCK_GEOLOCATION_COUNTRY=SV run world_otherworld 3 seed-world "${W[@]/TENANT_SLUG=acc-one/TENANT_SLUG=acc-three}" 
HARNESS_APP_ENV=production run world_prod 2 seed-world "${W[@]}"
run world_noreason 2 seed-world TENANT_SLUG=acc-one BRAND_SLUG=acc-one-brand COUNTRY=SV AUTH_REF=T OPERATOR=o

if [ "$fails" -ne 0 ]; then echo "${fails} assertion(s) failed; outputs kept in ${OUT}" >&2; trap 'bash "$H" down >/dev/null 2>&1 || true' EXIT; exit 1; fi
echo "all local runner mode assertions held"
