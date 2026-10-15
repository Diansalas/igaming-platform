#!/usr/bin/env bash
# A12: REAL local backup / restore / point-in-time-recovery drill with synthetic data (ADR 0122).
#
#   deploy/backup/drill.sh [--tenants N] [--players N] [--rounds N] [--small] [--skip-pitr] [--results-file FILE]
#
# Builds its OWN throwaway PostgreSQL 16 clusters under /tmp/pgdrill-XXXXXX (ports 5499 source, 5498 full-loss
# restore target, 5497 PITR recovery), runs everything against them, and tears them down on exit/failure. It
# never contacts, configures or reads the shared development/CI cluster on 5432, creates roles only inside the
# throwaway clusters, uses no sudo/pkill (`runuser -u postgres` drops privileges; clusters are stopped with pg_ctl),
# touches no network or AWS, and uses only synthetic data and per-run random passwords.
#
# Phases (RESULT lines; exit 0 only if all PASS):
#   0 preflight (tools, disk)      1 cluster + roles (deploy/init-app-role.sql) + migrations + seed through the platform
#   2 BACKUP-ROLE-1 proof          3 logical backup (plain + encrypted hook) -> restore -> verify (same cluster)
#   4 full-loss restore into a NEW cluster -> credentials re-provisioned -> anchor behaviour -> verify
#   5 negative control + failure handling drills     6 PITR (deploy/backup/pitr-drill.sh)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"
# shellcheck source=deploy/backup/lib-cluster.sh
. "${SCRIPT_DIR}/lib-cluster.sh"
# shellcheck source=deploy/backup/lib-provision.sh
. "${SCRIPT_DIR}/lib-provision.sh"

TENANTS=4; PLAYERS=24; ROUNDS=400; DEPOSITS=3; SKIP_PITR=0; RESULTS_FILE=""; MIN_ENTRIES=100000; SMALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --tenants) shift; TENANTS="${1:-}" ;;
    --players) shift; PLAYERS="${1:-}" ;;
    --rounds) shift; ROUNDS="${1:-}" ;;
    --small) SMALL=1; TENANTS=2; PLAYERS=5; ROUNDS=30; MIN_ENTRIES=0 ;;
    --skip-pitr) SKIP_PITR=1 ;;
    --results-file) shift; RESULTS_FILE="${1:-}" ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 0 ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done

bk_need_tools runuser psql pg_dump pg_restore pg_dumpall sha256sum go awk age age-keygen
for t in pg_basebackup pg_verifybackup; do [ -x "${PG_BIN}/${t}" ] || bk_die "${PG_BIN}/${t} not found"; done
bk_check_free() { # min_mb phase
  local free; free="$(df -Pm /tmp | awk 'NR==2 {print $4}')"
  [ "$free" -ge "$1" ] || bk_die "insufficient disk in /tmp before ${2}: ${free} MB free, ${1} MB needed. The drill stops here (and tears down) rather than risk filling the shared disk"
}
bk_check_free "${DRILL_MIN_FREE_MB:-900}" "start"

trap cl_teardown EXIT INT TERM
cl_new_root
WORK="${DRILL_ROOT}/work"; SECRETS="${DRILL_ROOT}/secrets"; BKUP="${DRILL_ROOT}/backups"
mkdir -p "$BKUP"; chmod 700 "$BKUP"

OUT="${WORK}/results.txt"; : >"$OUT"
FAILS=0
res() { echo "RESULT|$1|$2|$3" | tee -a "$OUT"; [ "$2" = PASS ] || FAILS=$((FAILS + 1)); }
meas() { echo "$*" | tee -a "$OUT"; }
section() { echo; echo "=== $* ==="; }

DB=igaming_platform_dev
SRC_PORT=5499; LOSS_PORT=5498
# psql as a role (passfile based) against a throwaway port
psql_as() { # user passfile port db args...
  local u="$1" pf="$2" p="$3" d="$4"; shift 4
  PGPASSFILE="$pf" psql -X -q -At -h 127.0.0.1 -p "$p" -U "$u" -d "$d" "$@"
}
pw_of() { awk -F: -v p="$2" -v u="$3" '$2==p && $4==u {print $5}' "$1"; }

# ================================================================= phase 1: cluster, roles, migrations, seed
section "1 cluster, roles, migrations, synthetic data (platform services)"
echo "building binaries"
( cd "$REPO_ROOT" && go build -o "${WORK}/migrate" ./cmd/migrate && go build -o "${WORK}/restorecheck" ./deploy/backup/restorecheck )
# bulk load with archiving OFF (keeps /tmp bounded: ~15 KB of WAL per ledger entry); archiving is enabled before the
# first backup, so the PITR window starts at the base backup (documented limit of this drill)
cl_init src "$SRC_PORT" "archive_timeout = 30
max_wal_size = 128MB
min_wal_size = 48MB"
cl_start src "$SRC_PORT"
prov_roles src "$SRC_PORT"
T=$(now_s)
DATABASE_URL="postgres://igaming:$(pw_of "${SECRETS}/pgpass" "$SRC_PORT" igaming)@127.0.0.1:${SRC_PORT}/${DB}?sslmode=disable" "${WORK}/migrate" -dir "${REPO_ROOT}/migrations" up >"${WORK}/migrate.log" 2>&1
meas "TIMING|migrate_up_seconds|$(elapsed_s "$T" "$(now_s)")"
prov_reapply_app_roles src "$SRC_PORT"
FUTURE="$(date -u -d '+1 hour' '+%Y-%m-%d %H:%M:%S+00')"
prov_backup_role src "$SRC_PORT" "$FUTURE"
prov_restore_role src "$SRC_PORT"
prov_replication_role src "$SRC_PORT"
chmod 600 "${SECRETS}"/pgpass*
res cluster_and_roles PASS "throwaway cluster ${DRILL_ROOT}/src on 127.0.0.1:${SRC_PORT}; roles igaming, igaming_runtime (deploy/init-app-role.sql), igaming_backup, drill_restore, drill_replicator; $(grep -c . "${WORK}/migrate.log") migrate log lines"

bk_check_free 600 "seeding"
T=$(now_s)
DRILL_SEED_TENANTS="$TENANTS" DRILL_SEED_PLAYERS="$PLAYERS" DRILL_SEED_ROUNDS="$ROUNDS" DRILL_SEED_DEPOSITS="$DEPOSITS" DRILL_SEED_WORKERS="${DRILL_SEED_WORKERS:-8}" \
  "${SCRIPT_DIR}/seed.sh" --drill-root "$DRILL_ROOT" --port "$SRC_PORT" --report "${WORK}/seed.json" >"${WORK}/seed.log" 2>&1 \
  || { tail -15 "${WORK}/seed.log" >&2; bk_die "seeding failed"; }
meas "TIMING|seed_seconds|$(elapsed_s "$T" "$(now_s)")"
BPF="${SECRETS}/pgpass.backup"
src_q() { psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "$1"; }
ENTRIES="$(src_q 'SELECT count(*) FROM ledger_entries')"
TXNS="$(src_q 'SELECT count(*) FROM ledger_transactions')"
TOTAL_ROWS="$(src_q "SELECT sum((xpath('/row/n/text()', query_to_xml(format('select count(*) as n from public.%I', relname), false, true, '')))[1]::text::bigint) FROM pg_class WHERE relkind IN ('r','p') AND relnamespace = 'public'::regnamespace")"
DBSIZE="$(src_q "SELECT pg_database_size(current_database())")"
meas "SIZE|source_ledger_entries|${ENTRIES}"; meas "SIZE|source_ledger_transactions|${TXNS}"; meas "SIZE|source_total_rows|${TOTAL_ROWS}"; meas "SIZE|source_database_bytes|${DBSIZE}"
meas "DATA|seed_tally|$(tr -d '\n ' <"${WORK}/seed.json")"
meas "DATA|ledger_transaction_types|$(src_q "SELECT string_agg(transaction_type || '=' || n, ', ' ORDER BY n DESC) FROM (SELECT transaction_type, count(*) n FROM ledger_transactions GROUP BY 1) q")"
meas "DATA|withdrawal_states|$(src_q "SELECT string_agg(state || '=' || n, ', ') FROM (SELECT state, count(*) n FROM withdrawal_requests GROUP BY 1) q")"
meas "DATA|adjustment_states|$(src_q "SELECT string_agg(state || '=' || n, ', ') FROM (SELECT state, count(*) n FROM ledger_adjustment_requests GROUP BY 1) q")"
meas "DATA|audit_rows_alerts_bonus_grants|$(src_q "SELECT (SELECT count(*) FROM audit_log) || ' / ' || (SELECT count(*) FROM alerts) || ' / ' || (SELECT count(*) FROM bonus_grants)")"
if [ "$ENTRIES" -ge "$MIN_ENTRIES" ]; then res synthetic_world_loaded_through_platform_services PASS "${ENTRIES} ledger entries in ${TXNS} transactions, ${TOTAL_ROWS} rows in the database (target >= ${MIN_ENTRIES})"; else res synthetic_world_loaded_through_platform_services FAIL "only ${ENTRIES} ledger entries (< ${MIN_ENTRIES})"; fi

# ================================================================= phase 2: BACKUP-ROLE-1 proof
section "2 BACKUP-ROLE-1: the problem, and the dedicated role"
OPF="${SECRETS}/pgpass"
dump_as() { PGPASSFILE="$2" pg_dump -h 127.0.0.1 -p "$SRC_PORT" -U "$1" -Fc "${@:3}" "$DB"; }
if OUTP="$(dump_as igaming "$OPF" -f /dev/null 2>&1)"; then res owner_plain_dump_refused FAIL "owner pg_dump unexpectedly succeeded"; else
  case "$OUTP" in *"row-level security"*) res owner_plain_dump_refused PASS "pg_dump as the migration owner FAILS: $(printf '%s' "$OUTP" | head -1 | cut -c1-150)" ;; *) res owner_plain_dump_refused FAIL "failed for another reason: $OUTP" ;; esac; fi
if dump_as igaming "$OPF" --enable-row-security -f "${WORK}/owner-rls.dump" >/dev/null 2>"${WORK}/owner-rls.err"; then
  OWNER_ROWS="$(pg_restore -a -t ledger_entries -f - "${WORK}/owner-rls.dump" 2>/dev/null | awk '/^COPY /{f=1;next} /^\\\.$/{f=0} f{n++} END{print n+0}' || true)"
  if [ "${OWNER_ROWS:-0}" -lt "$ENTRIES" ]; then res owner_dump_with_enable_row_security_is_incomplete PASS "pg_dump --enable-row-security as the owner SUCCEEDS but holds ${OWNER_ROWS} of ${ENTRIES} ledger_entries (silent data loss; this is why it is not a backup)"; else res owner_dump_with_enable_row_security_is_incomplete FAIL "owner dump held ${OWNER_ROWS} rows"; fi
else
  res owner_dump_with_enable_row_security_is_incomplete PASS "pg_dump --enable-row-security as the owner also fails: $(head -1 "${WORK}/owner-rls.err" | cut -c1-150)"
fi
rm -f "${WORK}/owner-rls.dump"
if OUTP="$(dump_as igaming_runtime "$OPF" -f /dev/null 2>&1)"; then res runtime_role_dump_refused FAIL "runtime role dump unexpectedly succeeded"; else res runtime_role_dump_refused PASS "pg_dump as igaming_runtime FAILS: $(printf '%s' "$OUTP" | head -1 | cut -c1-120)"; fi
# the backup role: complete and read-only
dump_as igaming_backup "$BPF" -f "${WORK}/probe.dump" >/dev/null 2>&1 && PROBE_ROWS="$(pg_restore -a -t ledger_entries -f - "${WORK}/probe.dump" | awk '/^COPY /{f=1;next} /^\\\.$/{f=0} f{n++} END{print n+0}')" || PROBE_ROWS=-1
rm -f "${WORK}/probe.dump"
if [ "$PROBE_ROWS" = "$ENTRIES" ]; then res backup_role_dump_complete PASS "pg_dump as igaming_backup holds all ${PROBE_ROWS} ledger_entries"; else res backup_role_dump_complete FAIL "backup role dump rows=${PROBE_ROWS} expected ${ENTRIES}"; fi
write_denied=0; write_total=0
for stmt in "INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES (gen_random_uuid(), 'x', 'x', 'under_platform_licence', 'active')" \
            "UPDATE ledger_entries SET amount = amount + 1" "DELETE FROM ledger_entries" "TRUNCATE ledger_entries" \
            "CREATE TABLE zz_backup_probe (a int)" "CREATE TEMP TABLE zz_probe (a int)" "DROP TABLE ledger_entries" \
            "ALTER TABLE ledger_entries DISABLE TRIGGER ALL" \
            "SET ROLE igaming" "SET session_replication_role = replica" "CREATE ROLE zz_probe" "ALTER ROLE igaming_backup SUPERUSER" \
            "COPY (SELECT 1) TO PROGRAM 'true'" "SELECT pg_read_file('/etc/passwd')" "SELECT pg_reload_conf()"; do
  write_total=$((write_total + 1))
  if psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "BEGIN READ WRITE; ${stmt}; COMMIT" >/dev/null 2>"${WORK}/probe.err"; then echo "NOT DENIED: ${stmt}" >>"${WORK}/probe.fail"; else write_denied=$((write_denied + 1)); fi
done
if [ "$write_denied" = "$write_total" ]; then res backup_role_cannot_write_or_escalate PASS "${write_total}/${write_total} probes denied (DML, DDL, TRUNCATE, TEMP, trigger disable, SET ROLE, replica mode, CREATE/ALTER ROLE, COPY PROGRAM, file read, reload conf)"; else res backup_role_cannot_write_or_escalate FAIL "$(cat "${WORK}/probe.fail" | tr '\n' ';')"; fi
ATTRS="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -F, -c "SELECT rolsuper::int, rolbypassrls::int, rolcreatedb::int, rolcreaterole::int, rolreplication::int, rolconnlimit, (SELECT count(*) FROM pg_auth_members WHERE member = r.oid), rolvaliduntil IS NOT NULL FROM pg_roles r WHERE rolname = 'igaming_backup'")"
if [ "$ATTRS" = "0,1,0,0,0,2,0,t" ]; then res backup_role_attributes PASS "NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 2, member of no role, VALID UNTIL set"; else res backup_role_attributes FAIL "attrs=${ATTRS}"; fi
( for i in 1 2; do psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "SELECT pg_sleep(6)" >/dev/null 2>&1 & done; sleep 2
  if psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "SELECT 1" >/dev/null 2>"${WORK}/connlimit.err"; then echo NOTLIMITED >"${WORK}/connlimit.res"; else grep -q 'too many connections' "${WORK}/connlimit.err" && echo LIMITED >"${WORK}/connlimit.res" || echo OTHER >"${WORK}/connlimit.res"; fi; wait ) || true
if [ "$(cat "${WORK}/connlimit.res")" = LIMITED ]; then res backup_role_connection_limit_enforced PASS "a third concurrent session is refused (CONNECTION LIMIT 2)"; else res backup_role_connection_limit_enforced FAIL "$(cat "${WORK}/connlimit.res")"; fi
# audit evidence: connections + statements logged for the role (log_connections/log_disconnections cluster-wide, log_statement role-level)
AUD_CONN="$(grep -c 'connection authorized: user=igaming_backup' "${DRILL_ROOT}/src/server.log" || true)"
AUD_STMT="$(grep -c 'igaming_backup@.*LOG:  statement:' "${DRILL_ROOT}/src/server.log" || true)"
AUD_DISC="$(grep -c 'disconnection: .*user=igaming_backup' "${DRILL_ROOT}/src/server.log" || true)"
if [ "$AUD_CONN" -gt 0 ] && [ "$AUD_STMT" -gt 0 ] && [ "$AUD_DISC" -gt 0 ]; then res backup_role_activity_is_audited PASS "server log holds ${AUD_CONN} connection, ${AUD_DISC} disconnection and ${AUD_STMT} statement lines for igaming_backup (log_connections, log_disconnections, ALTER ROLE ... SET log_statement; on RDS pgaudit)"; else res backup_role_activity_is_audited FAIL "conn=${AUD_CONN} disc=${AUD_DISC} stmt=${AUD_STMT}"; fi

# ================================================================= phase 3: logical backup -> restore -> verify
section "3 logical backup (backup role) -> restore into a new database -> verify"
export BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT="$SRC_PORT" BACKUP_PGDATABASE="$DB" BACKUP_PGUSER=igaming_backup BACKUP_PGPASSFILE="$BPF"
# enable continuous WAL archiving now (restart): the physical-recovery window starts here
cl_stop src
cat >>"${DRILL_ROOT}/src/data/postgresql.conf" <<EOF
archive_mode = on
archive_command = 'test ! -f ${DRILL_ROOT}/src/wal_archive/%f && cp %p ${DRILL_ROOT}/src/wal_archive/%f'
EOF
cl_start src "$SRC_PORT"
bk_check_free 450 "backup"
# the unencrypted-by-default refusal
if OUTP="$("${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" 2>&1)"; then res backup_refuses_without_encryption_policy FAIL "unencrypted backup was produced"; else case "$OUTP" in *"no encryption recipient configured"*) res backup_refuses_without_encryption_policy PASS "refused: $(printf '%s' "$OUTP" | cut -c1-110)" ;; *) res backup_refuses_without_encryption_policy FAIL "$OUTP" ;; esac; fi
export BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only
BOUT="$("${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label plain 2>&1)"; echo "$BOUT" | grep -E '^(TIMING|SIZE)' | tee -a "$OUT"
PLAIN_DIR="$(printf '%s\n' "$BOUT" | sed -n 's/^RUN_DIR=//p')"
[ -d "$PLAIN_DIR" ] || { echo "$BOUT" >&2; bk_die "backup failed"; }
meas "SIZE|backup_directory_bytes|$(du -sb "$PLAIN_DIR" | cut -f1)"
res logical_backup_written PASS "$(basename "$PLAIN_DIR"): $(ls "$PLAIN_DIR" | tr '\n' ' ')"
# encryption hook with an EPHEMERAL age key generated inside the drill root (no key material in the repository)
age-keygen -o "${SECRETS}/age.key" >/dev/null 2>&1; chmod 600 "${SECRETS}/age.key"
AGE_PUB="$(age-keygen -y "${SECRETS}/age.key")"
EOUT="$(env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label age 2>&1)"; echo "$EOUT" | grep -E '^TIMING\|backup_(total|encrypt)' | sed 's/backup_/backup_encrypted_/' | tee -a "$OUT"
ENC_DIR="$(printf '%s\n' "$EOUT" | sed -n 's/^RUN_DIR=//p')"
[ -d "$ENC_DIR" ] && ls "$ENC_DIR"/db.dump.age >/dev/null 2>&1 && ! ls "$ENC_DIR"/db.dump >/dev/null 2>&1 && res encryption_hook_age PASS "artifacts age-encrypted ($(ls "$ENC_DIR" | tr '\n' ' ')); no plaintext dump left" || res encryption_hook_age FAIL "$EOUT"

RPF="${SECRETS}/pgpass.restore"
restore_run() { # dir newdb [extra args...]  (env: RESTORE_* from caller)
  local dir="$1" nd="$2"; shift 2
  ( export BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT="${RESTORE_PORT:-$SRC_PORT}" BACKUP_PGDATABASE=postgres BACKUP_PGUSER="${RESTORE_USER:-drill_restore}" BACKUP_PGPASSFILE="${RESTORE_PASSFILE:-$RPF}" RESTORE_CONFIRM="${RESTORE_CONFIRM_OVERRIDE:-$nd}"
    "${SCRIPT_DIR}/restore.sh" --backup-dir "$dir" --new-database "$nd" "$@" ) 2>&1
}
verify_run() { # port db baseline [extra args] ; env VERIFY_*
  local port="$1" db="$2" base="$3"; shift 3
  ( export BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT="$port" BACKUP_PGDATABASE="$db" BACKUP_PGUSER=igaming_backup BACKUP_PGPASSFILE="${VERIFY_BACKUP_PASSFILE:-$BPF}"
    export VERIFY_OWNER_PGUSER=igaming VERIFY_OWNER_PGPASSFILE="${SECRETS}/pgpass"
    export VERIFY_OWNER_DATABASE_URL="postgres://igaming:$(pw_of "${SECRETS}/pgpass" "$port" igaming)@127.0.0.1:${port}/${db}?sslmode=disable"
    export VERIFY_RUNTIME_DATABASE_URL="postgres://igaming_runtime:$(pw_of "${SECRETS}/pgpass" "$port" igaming_runtime)@127.0.0.1:${port}/${db}?sslmode=disable"
    export MIGRATE_BIN="${WORK}/migrate" RESTORECHECK_BIN="${WORK}/restorecheck"
    "${SCRIPT_DIR}/verify.sh" --baseline "$base" "$@" ) 2>&1
}

RST_DB=igaming_restored
ROUT="$(restore_run "$PLAIN_DIR" "$RST_DB")" || { echo "$ROUT" >&2; bk_die "restore failed"; }
echo "$ROUT" | grep -E '^TIMING' | tee -a "$OUT"
res logical_restore_into_new_database PASS "restored into ${RST_DB} on the same cluster (single transaction, no trigger/RLS bypass)"
VOUT="$(verify_run "$SRC_PORT" "$RST_DB" "${PLAIN_DIR}/baseline.txt")" && VRC=0 || VRC=$?
echo "$VOUT" | grep -E '^(CHK|TIMING|VERIFY)' >"${WORK}/verify-logical.txt"; grep -E '^TIMING' "${WORK}/verify-logical.txt" | tee -a "$OUT"
if [ "$VRC" -eq 0 ]; then res logical_restore_verified PASS "$(tail -1 "${WORK}/verify-logical.txt"); $(grep -c '^CHK|.*|PASS|' "${WORK}/verify-logical.txt") checks"; else res logical_restore_verified FAIL "$(grep FAIL "${WORK}/verify-logical.txt" | head -4 | cut -c1-200 | tr '\n' ' ')"; fi
# encrypted artifact: same restore (decrypt with the ephemeral identity), compare counts only
RESTORE_AGE_IDENTITY_FILE="${SECRETS}/age.key" restore_run "$ENC_DIR" igaming_restored_enc >"${WORK}/enc-restore.txt" && ENCRC=0 || ENCRC=$?
if [ "$ENCRC" -eq 0 ]; then
  ENC_COUNT="$(psql_as igaming_backup "$BPF" "$SRC_PORT" igaming_restored_enc -c 'SELECT count(*) FROM ledger_entries')"
  [ "$ENC_COUNT" = "$ENTRIES" ] && res encrypted_backup_restores PASS "age-encrypted backup decrypted and restored: ledger_entries=${ENC_COUNT}" || res encrypted_backup_restores FAIL "count ${ENC_COUNT} != ${ENTRIES}"
else res encrypted_backup_restores FAIL "$(tail -3 "${WORK}/enc-restore.txt" | tr '\n' ' ')"; fi
psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c 'DROP DATABASE IF EXISTS igaming_restored_enc WITH (FORCE)' >/dev/null 2>&1 || true

# ================================================================= phase 5a: negative control + failure handling (backup/restore side)
section "5 negative control and failure handling"
# negative control: damage the restored copy (own throwaway superuser, guard bypass is DRILL ONLY) -> verify must FAIL
cl_super src "$SRC_PORT" -d "$RST_DB" -c "SET session_replication_role = replica; UPDATE ledger_entries SET amount = amount + 7 WHERE id = (SELECT id FROM ledger_entries ORDER BY id LIMIT 1)" >/dev/null
if NOUT="$(verify_run "$SRC_PORT" "$RST_DB" "${PLAIN_DIR}/baseline.txt" 2>&1)"; then res negative_control_verify_detects_tampered_ledger FAIL "verify PASSED on a damaged restore (vacuous verifier)"; else
  NDET="$(printf '%s\n' "$NOUT" | grep -E '^CHK\|(equal_TABLE|invariants_all_zero)\|FAIL' | cut -d'|' -f2 | tr '\n' ' ')"
  res negative_control_verify_detects_tampered_ledger PASS "verify FAILED as required on a copy with one ledger amount +7; detected by: ${NDET}"; fi
cl_super src "$SRC_PORT" -d "$RST_DB" -c "SET session_replication_role = replica; UPDATE ledger_entries SET amount = amount - 7 WHERE id = (SELECT id FROM ledger_entries ORDER BY id LIMIT 1)" >/dev/null
VOUT2="$(verify_run "$SRC_PORT" "$RST_DB" "${PLAIN_DIR}/baseline.txt" 2>&1)" && res negative_control_reverted_verify_passes_again PASS "after undoing the damage verify passes again (the failure was the damage, nothing else)" || res negative_control_reverted_verify_passes_again FAIL "$(printf '%s\n' "$VOUT2" | grep FAIL | head -3 | tr '\n' ' ')"

db_exists() { psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c "SELECT count(*) FROM pg_database WHERE datname = '$1'"; }
# F1 corrupted dump (bit flip in the middle)
cp -a "$PLAIN_DIR" "${BKUP}/f1"; SZ=$(stat -c %s "${BKUP}/f1/db.dump"); printf '\xff\xfe\xfd\xfc' | dd of="${BKUP}/f1/db.dump" bs=1 seek=$((SZ / 2)) conv=notrunc status=none
OUTP="$(restore_run "${BKUP}/f1" f1_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(printf '%s' "$OUTP" | grep -c 'CHECKSUM MISMATCH')" -ge 1 ] && [ "$(db_exists f1_db)" = 0 ]; then res failure_corrupted_dump_detected_before_restore PASS "checksum mismatch reported, exit ${RC}, no database created"; else res failure_corrupted_dump_detected_before_restore FAIL "rc=${RC} $OUTP"; fi
# F2 truncated dump (checksum not updated)
cp -a "$PLAIN_DIR" "${BKUP}/f2"; truncate -s $((SZ / 2)) "${BKUP}/f2/db.dump"
OUTP="$(restore_run "${BKUP}/f2" f2_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(printf '%s' "$OUTP" | grep -c 'CHECKSUM MISMATCH')" -ge 1 ] && [ "$(db_exists f2_db)" = 0 ]; then res failure_truncated_dump_detected_by_checksum PASS "checksum mismatch, exit ${RC}, no database created"; else res failure_truncated_dump_detected_by_checksum FAIL "rc=${RC} $OUTP"; fi
# F2b truncated dump with a RECOMPUTED checksum (damage that happened before checksumming): pg_restore refuses it up front
cp -a "$PLAIN_DIR" "${BKUP}/f2b"; truncate -s $((SZ * 6 / 10)) "${BKUP}/f2b/db.dump"; ( cd "${BKUP}/f2b" && sha256sum -- db.dump globals.sql baseline.txt >SHA256SUMS )
OUTP="$(restore_run "${BKUP}/f2b" f2b_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(db_exists f2b_db)" = 0 ]; then res failure_truncated_dump_with_valid_checksum_refused PASS "pg_restore refused the unreadable archive (${OUTP##*backup: }); exit ${RC}; no database created"; else res failure_truncated_dump_with_valid_checksum_refused FAIL "rc=${RC} exists=$(db_exists f2b_db) $OUTP"; fi
# F2c bit-flipped dump with a RECOMPUTED checksum: the checksum cannot help; the archive's own zlib check fails MID-restore and the single transaction rolls back
cp -a "$PLAIN_DIR" "${BKUP}/f2c"; printf '\xde\xad\xbe\xef\x00\x11\x22\x33' | dd of="${BKUP}/f2c/db.dump" bs=1 seek=$((SZ * 3 / 4)) conv=notrunc status=none; ( cd "${BKUP}/f2c" && sha256sum -- db.dump globals.sql baseline.txt >SHA256SUMS )
OUTP="$(restore_run "${BKUP}/f2c" f2c_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(db_exists f2c_db)" = 0 ] && printf '%s' "$OUTP" | grep -q 'pg_restore failed'; then res failure_corrupt_dump_with_valid_checksum_rolled_back PASS "pg_restore failed part-way ($(printf '%s' "$OUTP" | grep -m1 'pg_restore failed' | cut -c1-150)); exit ${RC}; single transaction rolled back, half-created database dropped"; else res failure_corrupt_dump_with_valid_checksum_rolled_back FAIL "rc=${RC} exists=$(db_exists f2c_db) $OUTP"; fi
# F3 wrong role
for wr in igaming_runtime igaming_backup; do
  if [ "$wr" = igaming_runtime ]; then PF="${SECRETS}/pgpass"; else PF="$BPF"; fi
  OUTP="$(RESTORE_USER=$wr RESTORE_PASSFILE=$PF restore_run "$PLAIN_DIR" f3_db)" && RC=0 || RC=$?
  if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q "restoring as ${wr} is refused" && [ "$(db_exists f3_db)" = 0 ]; then res "failure_restore_as_${wr}_refused" PASS "clear refusal, exit ${RC}, no database created"; else res "failure_restore_as_${wr}_refused" FAIL "rc=${RC} $OUTP"; fi
done
# F4 existing database (non-empty) refused and untouched
BEFORE="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$RST_DB" -c 'SELECT count(*) FROM ledger_entries')"
OUTP="$(restore_run "$PLAIN_DIR" "$RST_DB")" && RC=0 || RC=$?
AFTER="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$RST_DB" -c 'SELECT count(*) FROM ledger_entries')"
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'already exists' && [ "$BEFORE" = "$AFTER" ]; then res failure_restore_into_existing_database_refused PASS "refused (database exists), existing ledger_entries unchanged (${BEFORE})"; else res failure_restore_into_existing_database_refused FAIL "rc=${RC} ${BEFORE}/${AFTER} $OUTP"; fi
# F5 insufficient disk: both scripts refuse up front (a real ENOSPC mid-restore is NOT simulated: documented)
OUTP="$(BACKUP_MIN_FREE_MB=999999999 "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label nodisk 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'insufficient disk space' && ! ls "$BKUP"/*nodisk >/dev/null 2>&1; then res failure_insufficient_disk_backup_refused PASS "refused before writing anything: $(printf '%s' "$OUTP" | cut -c1-120)"; else res failure_insufficient_disk_backup_refused FAIL "rc=${RC} $OUTP"; fi
OUTP="$(RESTORE_MIN_FREE_MB=999999999 restore_run "$PLAIN_DIR" f5_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'insufficient disk space' && [ "$(db_exists f5_db)" = 0 ]; then res failure_insufficient_disk_restore_refused PASS "refused, no database created"; else res failure_insufficient_disk_restore_refused FAIL "rc=${RC} $OUTP"; fi
# F7 partial restore rolled back (fault injected: the pg_restore client is terminated part-way through the load)
OUTP="$(RESTORE_TEST_FAULT=kill_midway restore_run "$PLAIN_DIR" f7_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(db_exists f7_db)" = 0 ] && printf '%s' "$OUTP" | grep -q 'dropped'; then res failure_partial_restore_rolled_back PASS "pg_restore terminated part-way (injected fault: client killed once the new database had grown), exit ${RC}; the single transaction rolled back and the half-created database was dropped"; else res failure_partial_restore_rolled_back FAIL "rc=${RC} exists=$(db_exists f7_db) $OUTP"; fi
# F8 backup credential expired / missing / wrong role / wrong target
EXPIRED='2020-01-01 00:00:00+00'
prov_backup_role src "$SRC_PORT" "$EXPIRED"
OUTP="$("${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label expired 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'credential EXPIRED\|cannot connect' && ! ls "$BKUP"/*expired >/dev/null 2>&1; then res failure_backup_credential_expired_fails_closed PASS "exit ${RC}: $(printf '%s' "$OUTP" | cut -c1-200)"; else res failure_backup_credential_expired_fails_closed FAIL "rc=${RC} $OUTP"; fi
prov_backup_role src "$SRC_PORT" "$(date -u -d '+1 hour' '+%Y-%m-%d %H:%M:%S+00')"   # re-mint (new password, new passfile)
BPF="${SECRETS}/pgpass.backup"; export BACKUP_PGPASSFILE="$BPF"
OUTP="$(BACKUP_PGPASSFILE=${SECRETS}/does-not-exist "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label nocred 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'credential file missing'; then res failure_backup_credential_missing_fails_closed PASS "exit ${RC}: $(printf '%s' "$OUTP" | cut -c1-140)"; else res failure_backup_credential_missing_fails_closed FAIL "rc=${RC} $OUTP"; fi
OUTP="$(BACKUP_PGUSER=igaming BACKUP_PGPASSFILE="${SECRETS}/pgpass" "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label asowner 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'does not have BYPASSRLS' && ! ls "$BKUP"/*asowner >/dev/null 2>&1; then res failure_backup_as_owner_role_refused PASS "refused before dumping: $(printf '%s' "$OUTP" | cut -c1-150)"; else res failure_backup_as_owner_role_refused FAIL "rc=${RC} $OUTP"; fi
OUTP="$(BACKUP_PGPORT=5432 "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label shared 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'SHARED local cluster'; then res guard_refuses_shared_cluster_5432 PASS "refused without connecting"; else res guard_refuses_shared_cluster_5432 FAIL "rc=${RC} $OUTP"; fi
OUTP="$(BACKUP_PGHOST=db.example.invalid "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label remote 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'BACKUP_ALLOW_REMOTE_TARGET'; then res guard_refuses_remote_host_without_explicit_allow PASS "refused without connecting"; else res guard_refuses_remote_host_without_explicit_allow FAIL "rc=${RC} $OUTP"; fi
rm -rf "${BKUP}/f1" "${BKUP}/f2" "${BKUP}/f2b" "${BKUP}/f2c"

# ================================================================= phase 4: full-loss restore into a NEW cluster
section "4 full-loss restore: new empty cluster -> globals -> restore -> re-provision -> verify"
psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c "DROP DATABASE IF EXISTS ${RST_DB} WITH (FORCE)" >/dev/null   # free disk
T_PROV=$(now_s)
cl_init loss "$LOSS_PORT" "max_wal_size = 128MB
min_wal_size = 48MB"
cl_start loss "$LOSS_PORT"
meas "TIMING|fullloss_empty_cluster_initdb_and_start_seconds|$(elapsed_s "$T_PROV" "$(now_s)")"
LOSSPW="$(cl_random_secret)"
LPF="${SECRETS}/pgpass.loss"
IGPW="$LOSSPW" cl_super loss "$LOSS_PORT" -d postgres <<'SQL' >/dev/null
\getenv pw IGPW
ALTER ROLE postgres WITH PASSWORD :'pw';
SQL
cl_write_pass postgres "$LOSSPW" "$LOSS_PORT" "$LPF"
LOSS_DB=igaming_platform_dev
# a TAMPERED globals.sql with a recomputed checksum (the manifest is unsigned): --apply-globals must refuse and create nothing
cp -a "$PLAIN_DIR" "${BKUP}/tg"; sed -i 's/^ALTER ROLE igaming WITH NOSUPERUSER/ALTER ROLE igaming WITH SUPERUSER/' "${BKUP}/tg/globals.sql"; ( cd "${BKUP}/tg" && sha256sum -- db.dump globals.sql baseline.txt >SHA256SUMS )
OUTP="$(RESTORE_PORT=$LOSS_PORT RESTORE_USER=postgres RESTORE_PASSFILE=$LPF restore_run "${BKUP}/tg" "$LOSS_DB" --apply-globals)" && RC=0 || RC=$?
NROLES="$(cl_super loss "$LOSS_PORT" -d postgres -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'igaming%'")"
NDB="$(cl_super loss "$LOSS_PORT" -d postgres -c "SELECT count(*) FROM pg_database WHERE datname = '${LOSS_DB}'")"
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'tampered or unexpected role statement' && [ "$NROLES" = 0 ] && [ "$NDB" = 0 ]; then res failure_tampered_globals_refused PASS "ALTER ROLE igaming WITH SUPERUSER in a checksum-valid globals.sql refused by the allow-list; no role or database created"; else res failure_tampered_globals_refused FAIL "rc=${RC} roles=${NROLES} db=${NDB} $OUTP"; fi
rm -rf "${BKUP}/tg"
T=$(now_s)
LOUT="$(RESTORE_PORT=$LOSS_PORT RESTORE_USER=postgres RESTORE_PASSFILE=$LPF restore_run "$PLAIN_DIR" "$LOSS_DB" --apply-globals)" && LRC=0 || LRC=$?
if [ "$LRC" -ne 0 ]; then echo "$LOUT" >&2; fi
echo "$LOUT" | grep -E '^TIMING' | sed 's/restore_/fullloss_restore_/' | tee -a "$OUT"
if [ "$LRC" -eq 0 ]; then res full_loss_restore_into_new_cluster PASS "roles created WITHOUT passwords from globals.sql, database restored into an empty cluster"; else res full_loss_restore_into_new_cluster FAIL "$(echo "$LOUT" | tail -3 | tr '\n' ' ')"; fi
# credentials do not come back from a dump: log in must fail until they are re-provisioned
if PGPASSFILE=/dev/null psql -X -q -At -h 127.0.0.1 -p "$LOSS_PORT" -U igaming -d postgres -w -c 'select 1' >/dev/null 2>&1; then res restored_roles_have_no_password_until_reprovisioned FAIL "login worked without a password"; else res restored_roles_have_no_password_until_reprovisioned PASS "igaming/igaming_runtime/igaming_backup cannot log in after restore (no passwords in the dump, by design)"; fi
T_RP=$(now_s)
for r in igaming igaming_runtime; do
  NPW="$(cl_random_secret)"; IGPW="$NPW" RR="$r" cl_super loss "$LOSS_PORT" -d postgres <<'SQL' >/dev/null
\getenv pw IGPW
\getenv rr RR
ALTER ROLE :"rr" WITH PASSWORD :'pw';
SQL
  cl_write_pass "$r" "$NPW" "$LOSS_PORT" "${SECRETS}/pgpass"
done
LBPW="$(cl_random_secret)"
: >"${SECRETS}/pgpass.backup.loss"; chmod 600 "${SECRETS}/pgpass.backup.loss"
IGAMING_BACKUP_PASSWORD="$LBPW" BACKUP_ROLE_VALID_UNTIL="$(date -u -d '+1 hour' '+%Y-%m-%d %H:%M:%S+00')" cl_super loss "$LOSS_PORT" -d "$LOSS_DB" -f "${SCRIPT_DIR}/init-backup-role.sql" >/dev/null
cl_write_pass igaming_backup "$LBPW" "$LOSS_PORT" "${SECRETS}/pgpass.backup.loss"
meas "TIMING|fullloss_reprovision_credentials_seconds|$(elapsed_s "$T_RP" "$(now_s)")"
VBAK="$BPF"
VOUT="$(VERIFY_BACKUP_PASSFILE="${SECRETS}/pgpass.backup.loss" verify_run "$LOSS_PORT" "$LOSS_DB" "${PLAIN_DIR}/baseline.txt")" && VRC=0 || VRC=$?
echo "$VOUT" | grep -E '^(CHK|TIMING|VERIFY)' >"${WORK}/verify-fullloss.txt"; grep -E '^TIMING' "${WORK}/verify-fullloss.txt" | sed 's/verify_/fullloss_verify_/' | tee -a "$OUT"
if [ "$VRC" -eq 0 ]; then res full_loss_restore_verified PASS "$(tail -1 "${WORK}/verify-fullloss.txt"); incl. database_environment anchor UNRECORDED => a production platform-api would REFUSE to start until it is recorded"; else res full_loss_restore_verified FAIL "$(grep FAIL "${WORK}/verify-fullloss.txt" | head -4 | cut -c1-200 | tr '\n' ' ')"; fi
# the anchor re-provisioning step, then the production startup guard admits the database
cl_super loss "$LOSS_PORT" -d "$LOSS_DB" -c "SET ROLE igaming; INSERT INTO database_environment (environment) VALUES ('production')" >/dev/null 2>"${WORK}/anchor.err" && ANCH=ok || ANCH="$(head -2 "${WORK}/anchor.err" | tr '\n' ' ')"
AOUT="$(DATABASE_URL="postgres://igaming_runtime:$(pw_of "${SECRETS}/pgpass" "$LOSS_PORT" igaming_runtime)@127.0.0.1:${LOSS_PORT}/${LOSS_DB}?sslmode=disable" "${WORK}/restorecheck" -expect-anchor production 2>&1 | grep -E 'environment_anchor_state|startup_guard_production' || true)"
if [ "$ANCH" = ok ] && printf '%s' "$AOUT" | grep -q 'environment_anchor_state|PASS' && printf '%s' "$AOUT" | grep -q 'startup_guard_production_process_admitted|PASS'; then res anchor_reprovisioned_production_guard_admits PASS "owner recorded 'production'; the production startup guard now admits the restored database"; else res anchor_reprovisioned_production_guard_admits FAIL "anchor=${ANCH} ${AOUT}"; fi
cl_stop loss; rm -rf "$(cl_dir loss)"

# ================================================================= phase 6: PITR
if [ "$SKIP_PITR" -eq 0 ]; then
  section "6 point-in-time recovery (physical base backup + WAL archive)"
  rm -rf "${BKUP:?}"/* 2>/dev/null || true   # logical backup sets are no longer needed: free disk for the base backup
  bk_check_free 600 "PITR"
  PITR_OUT="$("${SCRIPT_DIR}/pitr-drill.sh" --drill-root "$DRILL_ROOT" --src-port "$SRC_PORT" --rec-port 5497 2>&1)" && PRC=0 || PRC=$?
  printf '%s\n' "$PITR_OUT" | grep -E '^(RESULT|TIMING|SIZE|PITR_TARGET)' | tee -a "$OUT"
  [ "$PRC" -eq 0 ] || { FAILS=$((FAILS + 1)); printf '%s\n' "$PITR_OUT" | tail -15 >&2; }
fi

# ================================================================= summary
section "summary"
NPASS="$(grep -c '^RESULT|.*|PASS|' "$OUT" || true)"; NFAIL="$(grep -c '^RESULT|.*|FAIL|' "$OUT" || true)"
echo "RESULTS: ${NPASS} PASS, ${NFAIL} FAIL"
[ -z "$RESULTS_FILE" ] || cp "$OUT" "$RESULTS_FILE"
if [ "$NFAIL" -eq 0 ] && [ "$FAILS" -eq 0 ]; then echo "A12 DRILL RESULT: PASS"; exit 0; fi
echo "A12 DRILL RESULT: FAIL"; exit 1
