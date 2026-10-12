#!/usr/bin/env bash
# Local backup + restore DRILL (OPS-READINESS-LOCAL). Runbook: docs/runbooks/restore-from-backup.md;
# status record: docs/runbooks/backup-and-disaster-recovery.md.
#
# WHAT THIS IS: a scripted, repeatable exercise against PRIVATE scratch databases on a LOCAL
# development PostgreSQL, using synthetic data only. It produces evidence for THIS dev dataset only.
# The numbers it prints are NOT production RPO/RTO and it proves nothing about managed backups,
# PITR, replicas or cross-region copies (all AWS-gated, NOT MET; no AWS call is made here).
#
# What it does
#   1. creates a private source database (owner role + runtime-role grants, like the CI harness),
#      applies every migration (cmd/migrate up) and seeds synthetic tenants/players/wallets/ledger;
#   2. records a verification baseline (row counts per RLS scope, ledger invariant, projection ==
#      recompute, RLS enabled+FORCED, runtime-role pins, migration head, pg_catalog fingerprints);
#   3. LOGICAL path: pg_dump -Fc --schema-only (+ schema_migrations data) and pg_restore into a second
#      scratch database, then re-applies the database-level privileges a dump cannot carry
#      (deploy/drill/post-restore.sql) and verifies structure and `migrate verify`;
#      it also records that a FULL-DATA pg_dump by the migration-owner role is REFUSED by FORCE RLS
#      (finding BACKUP-ROLE-1: a data-bearing logical dump needs a dedicated RLS-bypassing backup role,
#      which this drill is forbidden to create);
#   4. SNAPSHOT path: CREATE DATABASE ... TEMPLATE (same cluster, same failure domain: NOT a backup,
#      only a data-bearing restore exercise) and verifies every check against the baseline, including
#      `migrate verify`; then shows the RPO of a snapshot by writing one more row to the source;
#   5. NEGATIVE CONTROL: damages the restored copy and requires the verifiers to FAIL;
#   6. drops every scratch database it created and removes its dump files.
#
# What it never does: ALTER/CREATE ROLE, change a password, sudo, pkill, touch the shared CI/dev
# database, lift RLS/FORCE on anything, call AWS, or connect to a non-local host. Base backup /
# PITR (pg_basebackup, WAL archiving) is NOT attempted: it needs the REPLICATION attribute or a
# restart with archive_mode, i.e. role/config changes this drill must not make (see the runbook).
#
# Usage
#   deploy/drill/backup-restore-drill.sh [--dry-run] [--deposits N] [--keep] [--cleanup-stale] [--help]
#
# Required environment (existing CI/dev harness credentials, never defaulted or printed):
#   TEST_ADMIN_DATABASE_URL  igaming_test_admin (CREATEDB, NOSUPERUSER) - CREATE/DROP DATABASE only
#   TEST_DATABASE_URL        the migration-owner role `igaming`; only user/password/host are used,
#                            the database name is replaced by the drill's private names
# Optional: DRILL_WORKDIR (parent dir for the temporary dump files; default $TMPDIR or /tmp).
#
# Exit status: 0 only if every gating check passed; 1 otherwise; 2 on usage/preflight refusal.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
DB_PREFIX="drill_bk_"

DRY_RUN=0
KEEP=0
CLEAN_STALE=0
DEPOSITS=50

usage() { sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --keep) KEEP=1 ;;
    --cleanup-stale) CLEAN_STALE=1 ;;
    --deposits) shift; DEPOSITS="${1:-}" ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

die() { echo "drill: $*" >&2; exit 2; }
case "$DEPOSITS" in ''|*[!0-9]*) die "--deposits must be a positive integer" ;; esac
[ "$DEPOSITS" -ge 1 ] && [ "$DEPOSITS" -le 5000 ] || die "--deposits must be between 1 and 5000 (disk is small; keep dumps small)"

: "${TEST_ADMIN_DATABASE_URL:?TEST_ADMIN_DATABASE_URL is required (igaming_test_admin harness credential)}"
: "${TEST_DATABASE_URL:?TEST_DATABASE_URL is required (migration-owner harness credential)}"

for tool in psql pg_dump pg_restore go sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || die "required tool not found: $tool"
done
for f in seed verify post-restore post-backup-write tamper; do
  [ -f "${SCRIPT_DIR}/${f}.sql" ] || die "missing ${SCRIPT_DIR}/${f}.sql"
done
[ -d "${REPO_ROOT}/migrations" ] || die "migrations directory not found at ${REPO_ROOT}/migrations"

# shellcheck source=deploy/drill/lib-localdb.sh
WORK_PREFIX=drill-bk
. "${SCRIPT_DIR}/lib-localdb.sh"

SUFFIX="$(date +%s)_$$"
SRC_DB="${DB_PREFIX}src_${SUFFIX}"
LGC_DB="${DB_PREFIX}lgc_${SUFFIX}"
RST_DB="${DB_PREFIX}rst_${SUFFIX}"
for n in "$SRC_DB" "$LGC_DB" "$RST_DB"; do
  [[ "$n" =~ ^drill_bk_[a-z0-9_]+$ ]] || die "internal: unsafe scratch database name $n"
done
SRC_URL="$(url_with_db "$TEST_DATABASE_URL" "$SRC_DB")"
LGC_URL="$(url_with_db "$TEST_DATABASE_URL" "$LGC_DB")"
RST_URL="$(url_with_db "$TEST_DATABASE_URL" "$RST_DB")"

if [ "$CLEAN_STALE" -eq 1 ]; then
  # only databases this drill created (name prefix), old enough (the epoch is in the name), and with no
  # active connection; never anything else. DRILL_STALE_MIN_AGE_SECONDS (default 3600) tunes the age.
  MIN_AGE="${DRILL_STALE_MIN_AGE_SECONDS:-3600}"
  case "$MIN_AGE" in ''|*[!0-9]*) die "DRILL_STALE_MIN_AGE_SECONDS must be an integer" ;; esac
  NOW_EPOCH="$(date +%s)"
  while IFS='|' read -r d conns; do
    [[ "$d" =~ ^drill_bk_[a-z0-9_]+$ ]] || continue
    if [[ "$d" =~ _([0-9]{9,})_[0-9]+$ ]]; then ep="${BASH_REMATCH[1]}"; else echo "drill: skipping $d (no timestamp in name)"; continue; fi
    if [ $((NOW_EPOCH - ep)) -lt "$MIN_AGE" ]; then echo "drill: skipping $d (younger than ${MIN_AGE}s)"; continue; fi
    if [ "$conns" -gt 0 ]; then echo "drill: skipping $d (${conns} active connection(s))"; continue; fi
    echo "drill: dropping stale scratch database $d"
    adm -c "DROP DATABASE IF EXISTS \"$d\""
  done < <(adm -c "SELECT d.datname || '|' || (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname) FROM pg_database d WHERE d.datname LIKE 'drill\\_bk\\_%'")
  [ "$DRY_RUN" -eq 1 ] || exit 0
fi

# ---- dry run: validate everything that can be validated without creating a database ------------
if [ "$DRY_RUN" -eq 1 ]; then
  echo "drill: DRY RUN - no database is created or modified"
  bash -n "${BASH_SOURCE[0]}" && echo "drill: script syntax OK"
  adm -c "SELECT 'admin role=' || current_user || ' createdb=' || rolcreatedb || ' super=' || rolsuper || ' replication=' || rolreplication FROM pg_roles WHERE rolname = current_user"
  adm -c "SELECT 'server=' || current_setting('server_version') || ' wal_level=' || current_setting('wal_level') || ' archive_mode=' || current_setting('archive_mode')"
  adm -c "SELECT 'runtime role present=' || (count(*) = 1) FROM pg_roles WHERE rolname = 'igaming_runtime'"
  echo "drill: would create ${SRC_DB}, ${LGC_DB}, ${RST_DB}; seed ${DEPOSITS} deposits per wallet; then drop all three"
  echo "drill: dry run complete"
  exit 0
fi

# ---- working state, cleanup ---------------------------------------------------------------------
CREATED=()
cleanup() {
  local rc=$?
  set +e
  if [ "$KEEP" -eq 1 ]; then
    echo "drill: --keep set; scratch databases left: ${CREATED[*]:-none}; work dir: ${WORK}"
  else
    for d in "${CREATED[@]:-}"; do
      [ -n "$d" ] && [[ "$d" =~ ^drill_bk_[a-z0-9_]+$ ]] && adm -c "DROP DATABASE IF EXISTS \"$d\" WITH (FORCE)" >/dev/null 2>&1
    done
    rm -rf "$WORK"
  fi
  exit "$rc"
}
trap cleanup EXIT

now() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }
declare -A T
RESULTS=()
FAILS=0
record() { # name status detail
  RESULTS+=("$1|$2|$3")
  [ "$2" = "FAIL" ] && FAILS=$((FAILS + 1))
  return 0
}

STRUCT_FP="tables_md5 columns_md5 triggers_md5 functions_md5 policies_md5 constraints_md5 indexes_md5 object_acls_md5 default_acls_md5 schema_acl migration_head migration_checksums_md5 secdef_functions_without_search_path_pin"
DATA_CHK="rowcounts_nonempty ledger_debits_eq_credits_per_tenant_asset balance_projection_equals_ledger_recompute"
in_list() { case " $2 " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

run_verify() { # url outfile
  if ! psql -X -At -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/verify.sql" "$1" 2>"$2.err" | grep -E '^(CHK|FP|ROW)\|' >"$2"; then
    echo "drill: verify.sql failed to run:" >&2; cat "$2.err" >&2; return 1
  fi
  [ ! -s "$2.err" ] || { echo "drill: verify.sql errors:" >&2; cat "$2.err" >&2; return 1; }
}

# gate_checks <label> <verify-output> <mode: all|struct>: every CHK line of the file must be PASS.
gate_checks() {
  local label="$1" file="$2" mode="$3" name status detail line bad=0 n=0
  while IFS= read -r line; do
    IFS='|' read -r _ name status detail <<<"$line"
    if [ "$mode" = struct ] && in_list "$name" "$DATA_CHK"; then continue; fi
    n=$((n + 1))
    if [ "$status" = PASS ]; then record "${label}:${name}" PASS "$detail"; else record "${label}:${name}" FAIL "$detail"; bad=$((bad + 1)); fi
  done < <(grep '^CHK|' "$file")
  [ "$n" -gt 0 ] || { record "${label}:checks_ran" FAIL "no CHK lines produced (verifier did not run?)"; }
}

# compare_fp <label> <baseline> <candidate> <mode>: fingerprints (and, for mode all, row counts) must be equal.
compare_fp() {
  local label="$1" base="$2" cand="$3" mode="$4" name a b
  while IFS= read -r line; do
    IFS='|' read -r _ name _ <<<"$line"
    if [ "$mode" = struct ] && ! in_list "$name" "$STRUCT_FP"; then continue; fi
    a="$(grep "^FP|${name}|" "$base" | head -1)"; b="$(grep "^FP|${name}|" "$cand" | head -1)"
    if [ -n "$a" ] && [ "$a" = "$b" ]; then record "${label}:fingerprint:${name}" PASS "equal to source"
    else record "${label}:fingerprint:${name}" FAIL "source='${a#FP|${name}|}' restored='${b#FP|${name}|}'"; fi
  done < <(grep '^FP|' "$base")
  if [ "$mode" = all ]; then
    if diff -q <(grep '^ROW|' "$base") <(grep '^ROW|' "$cand") >/dev/null; then
      record "${label}:row_counts_all_tables_all_scopes" PASS "$(grep -c '^ROW|' "$base") non-zero (table,scope) counts identical"
    else
      record "${label}:row_counts_all_tables_all_scopes" FAIL "$(diff <(grep '^ROW|' "$base") <(grep '^ROW|' "$cand") | head -5 | tr '\n' ' ')"
    fi
  fi
}

MIGRATE_BIN="${WORK}/migrate"
migrate_verify() { # label url
  local out rc=0
  out="$(DATABASE_URL="$2" "$MIGRATE_BIN" -dir "${REPO_ROOT}/migrations" verify 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ]; then record "$1:migrate_verify" PASS "$(printf '%s' "$out" | tail -1)"
  else record "$1:migrate_verify" FAIL "exit=${rc} $(printf '%s' "$out" | tail -1)"; fi
  return 0
}

T[start]="$(now)"
echo "drill: building cmd/migrate"
( cd "$REPO_ROOT" && go build -o "$MIGRATE_BIN" ./cmd/migrate )

# ---- 1. private source database ----------------------------------------------------------------
echo "drill: [1/6] creating private source database ${SRC_DB} from migrations"
CREATED+=("$SRC_DB")
adm -c "CREATE DATABASE \"${SRC_DB}\" OWNER igaming"
psql -X -q -v ON_ERROR_STOP=1 "$SRC_URL" >/dev/null <<SQL
GRANT CONNECT ON DATABASE "${SRC_DB}" TO igaming_runtime;
GRANT USAGE ON SCHEMA public TO igaming_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE igaming IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE igaming IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime;
SQL
DATABASE_URL="$SRC_URL" "$MIGRATE_BIN" -dir "${REPO_ROOT}/migrations" up >/dev/null
# same post-migration narrowing the CI harness applies
psql -X -q -v ON_ERROR_STOP=1 "$SRC_URL" >/dev/null <<SQL
REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime;
REVOKE UPDATE, DELETE, TRUNCATE ON sportsbook_bet_settlements FROM igaming_runtime;
SQL
T[built]="$(now)"

echo "drill: seeding synthetic data (${DEPOSITS} deposits per wallet)"
psql -X -q -v ON_ERROR_STOP=1 -v seed_deposits="$DEPOSITS" -f "${SCRIPT_DIR}/seed.sql" "$SRC_URL" >/dev/null
T[seeded]="$(now)"

# ---- 2. baseline --------------------------------------------------------------------------------
echo "drill: [2/6] verifying the SOURCE (baseline)"
run_verify "$SRC_URL" "${WORK}/src.out"
gate_checks "source" "${WORK}/src.out" all
SRC_BYTES="$(adm -c "SELECT pg_database_size('${SRC_DB}')")"
SRC_LEDGER_ROWS="$(grep -c '^ROW|ledger_entries|' "${WORK}/src.out" || true)"
SRC_TOTAL_ROWS="$(grep '^CHK|rowcounts_nonempty|' "${WORK}/src.out" | sed -n 's/.*total_rows=\([0-9]*\).*/\1/p')"
SRC_LEDGER_ENTRIES="$(awk -F'|' '$1=="ROW" && $2=="ledger_entries" {s+=$4} END {print s+0}' "${WORK}/src.out")"
T[baseline]="$(now)"

# ---- 3. logical path ----------------------------------------------------------------------------
echo "drill: [3/6] LOGICAL: full-data pg_dump as the migration owner (expected: refused by FORCE RLS)"
FULL_RC=0
pg_dump -Fc -f /dev/null "$SRC_URL" 2>"${WORK}/full-dump.err" || FULL_RC=$?
if [ "$FULL_RC" -ne 0 ] && grep -q 'row-level security' "${WORK}/full-dump.err"; then
  record "logical:full_data_dump_as_owner" "REFUSED" "pg_dump exit=${FULL_RC}: FORCE RLS blocks the owner role, which has no RLS-bypass attribute (BACKUP-ROLE-1); not worked around"
elif [ "$FULL_RC" -eq 0 ]; then
  record "logical:full_data_dump_as_owner" "UNEXPECTED" "pg_dump succeeded as the owner; revisit docs/runbooks/restore-from-backup.md (this drill still uses the schema-only path)"
else
  record "logical:full_data_dump_as_owner" "FAIL" "pg_dump failed for another reason: $(head -1 "${WORK}/full-dump.err")"
fi

echo "drill: LOGICAL: schema-only dump + schema_migrations data, restore into ${LGC_DB}"
T[lgc0]="$(now)"
pg_dump -Fc --schema-only -f "${WORK}/schema.dump" "$SRC_URL"
pg_dump -Fc --data-only -t public.schema_migrations -f "${WORK}/migrations-data.dump" "$SRC_URL"
T[lgc_dumped]="$(now)"
SCHEMA_DUMP_BYTES="$(stat -c %s "${WORK}/schema.dump")"
SCHEMA_DUMP_SHA="$(sha256sum "${WORK}/schema.dump" | cut -d' ' -f1)"
CREATED+=("$LGC_DB")
adm -c "CREATE DATABASE \"${LGC_DB}\" OWNER igaming"
pg_restore --exit-on-error --no-owner --dbname "$LGC_URL" "${WORK}/schema.dump"
pg_restore --exit-on-error --no-owner --dbname "$LGC_URL" "${WORK}/migrations-data.dump"
T[lgc_restored]="$(now)"
run_verify "$LGC_URL" "${WORK}/lgc-raw.out"
RAW_TEMP="$(grep '^CHK|runtime_role_cannot_create_temp_0116|' "${WORK}/lgc-raw.out" | cut -d'|' -f3)"
if [ "$RAW_TEMP" = FAIL ]; then
  record "logical:raw_restore_shows_database_acl_loss" PASS "raw restore left TEMPORARY on PUBLIC (runtime_role_cannot_create_temp_0116=FAIL) as documented; the verifier detects the loss"
else
  record "logical:raw_restore_shows_database_acl_loss" FAIL "raw restore did NOT show the expected TEMP loss (got '${RAW_TEMP:-missing}'): the dump carried the ACL, or the verifier is vacuous"
fi
psql -X -q -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/post-restore.sql" "$LGC_URL" >/dev/null
run_verify "$LGC_URL" "${WORK}/lgc.out"
gate_checks "logical-schema" "${WORK}/lgc.out" struct
compare_fp "logical-schema" "${WORK}/src.out" "${WORK}/lgc.out" struct
migrate_verify "logical-schema" "$LGC_URL"
T[lgc_done]="$(now)"

# ---- 4. snapshot path ---------------------------------------------------------------------------
echo "drill: [4/6] SNAPSHOT: CREATE DATABASE ... TEMPLATE (same cluster; not a backup) -> ${RST_DB}"
T[rst0]="$(now)"
CREATED+=("$RST_DB")
adm -c "CREATE DATABASE \"${RST_DB}\" OWNER igaming TEMPLATE \"${SRC_DB}\""
T[rst_cloned]="$(now)"
run_verify "$RST_URL" "${WORK}/rst-raw.out"
RAW2_TEMP="$(grep '^CHK|runtime_role_cannot_create_temp_0116|' "${WORK}/rst-raw.out" | cut -d'|' -f3)"
if [ "$RAW2_TEMP" = FAIL ]; then
  record "snapshot:raw_clone_shows_database_acl_loss" PASS "raw clone left TEMPORARY on PUBLIC (runtime_role_cannot_create_temp_0116=FAIL) as documented; the verifier detects the loss"
else
  record "snapshot:raw_clone_shows_database_acl_loss" FAIL "raw clone did NOT show the expected TEMP loss (got '${RAW2_TEMP:-missing}'): the verifier is vacuous or the template carried the ACL"
fi
psql -X -q -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/post-restore.sql" "$RST_URL" >/dev/null
run_verify "$RST_URL" "${WORK}/rst.out"
gate_checks "snapshot" "${WORK}/rst.out" all
compare_fp "snapshot" "${WORK}/src.out" "${WORK}/rst.out" all
migrate_verify "snapshot" "$RST_URL"
T[rst_done]="$(now)"

# ---- 5. RPO demonstration -----------------------------------------------------------------------
echo "drill: [5/6] RPO demonstration: one more committed write to the source AFTER the snapshot"
psql -X -q -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/post-backup-write.sql" "$SRC_URL" >/dev/null
run_verify "$SRC_URL" "${WORK}/src2.out"
SRC2_LEDGER_ENTRIES="$(awk -F'|' '$1=="ROW" && $2=="ledger_entries" {s+=$4} END {print s+0}' "${WORK}/src2.out")"
RST_LEDGER_ENTRIES="$(awk -F'|' '$1=="ROW" && $2=="ledger_entries" {s+=$4} END {print s+0}' "${WORK}/rst.out")"
LOST=$((SRC2_LEDGER_ENTRIES - RST_LEDGER_ENTRIES))
if [ "$LOST" -gt 0 ]; then
  record "rpo:snapshot_loses_post_snapshot_writes" "PASS" "source has ${SRC2_LEDGER_ENTRIES} ledger entries, restored ${RST_LEDGER_ENTRIES}: ${LOST} committed entries absent (snapshot RPO > 0; RPO 0 needs WAL archiving / managed PITR, NOT MET)"
else
  record "rpo:snapshot_loses_post_snapshot_writes" "FAIL" "expected the post-snapshot write to be absent from the restored copy"
fi

# ---- 6. negative control ------------------------------------------------------------------------
echo "drill: [6/6] NEGATIVE CONTROL: damaging the restored scratch copy; the verifiers must now FAIL"
psql -X -q -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/tamper.sql" "$RST_URL" >/dev/null
run_verify "$RST_URL" "${WORK}/tampered.out"
EXPECT_FAIL="rls_enabled_and_forced_on_all_tenant_tables runtime_role_cannot_create_temp_0116 runtime_role_no_truncate_trigger_references_ddl ledger_debits_eq_credits_per_tenant_asset balance_projection_equals_ledger_recompute"
for c in $EXPECT_FAIL; do
  s="$(grep "^CHK|${c}|" "${WORK}/tampered.out" | cut -d'|' -f3)"
  if [ "$s" = FAIL ]; then record "negative-control:verify_detects:${c}" PASS "verifier reported FAIL on the damaged copy"
  else record "negative-control:verify_detects:${c}" FAIL "verifier reported '${s:-missing}' on a damaged copy (check is vacuous)"; fi
done
if ! diff -q <(grep '^ROW|' "${WORK}/src.out") <(grep '^ROW|' "${WORK}/tampered.out") >/dev/null \
   && [ "$(grep '^FP|rowcounts_md5|' "${WORK}/src.out")" != "$(grep '^FP|rowcounts_md5|' "${WORK}/tampered.out")" ]; then
  record "negative-control:row_count_diff_detects_deleted_row" PASS "non-zero (table,scope) counts and rowcounts_md5 differ from the source"
else
  record "negative-control:row_count_diff_detects_deleted_row" FAIL "row-count comparison did not notice a deleted projection row"
fi
t_fp="$(grep '^FP|triggers_md5|' "${WORK}/tampered.out" | head -1)"; b_fp="$(grep '^FP|triggers_md5|' "${WORK}/src.out" | head -1)"
if [ -n "$t_fp" ] && [ "$t_fp" != "$b_fp" ]; then record "negative-control:fingerprint_detects_dropped_trigger" PASS "triggers_md5 differs from source"
else record "negative-control:fingerprint_detects_dropped_trigger" FAIL "triggers_md5 unchanged after a trigger was dropped"; fi
nc_rc=0; DATABASE_URL="$RST_URL" "$MIGRATE_BIN" -dir "${REPO_ROOT}/migrations" verify >/dev/null 2>&1 || nc_rc=$?
if [ "$nc_rc" -ne 0 ]; then record "negative-control:migrate_verify_detects_checksum_tamper" PASS "migrate verify exit=${nc_rc}"
else record "negative-control:migrate_verify_detects_checksum_tamper" FAIL "migrate verify passed on a tampered checksum"; fi
T[end]="$(now)"

# ---- report -------------------------------------------------------------------------------------
echo
echo "================ OPS-READINESS-LOCAL backup/restore drill ================"
echo "scratch databases: ${SRC_DB} ${LGC_DB} ${RST_DB} (dropped on exit unless --keep)"
printf '%-72s %-9s %s\n' "CHECK" "RESULT" "DETAIL"
for r in "${RESULTS[@]}"; do
  IFS='|' read -r n s d <<<"$r"
  printf '%-72s %-9s %s\n' "$n" "$s" "$d"
done
echo
echo "---- measurements: THIS synthetic dev dataset only. NOT production RPO/RTO. ----"
echo "dataset: source database ${SRC_BYTES} bytes; ${SRC_TOTAL_ROWS:-?} RLS-visible rows; ${SRC_LEDGER_ENTRIES} ledger entries; ${DEPOSITS} deposits per wallet"
echo "migrate up + grants:                 $(elapsed "${T[start]}" "${T[built]}") s"
echo "seed:                                $(elapsed "${T[built]}" "${T[seeded]}") s"
echo "baseline verification (source):      $(elapsed "${T[seeded]}" "${T[baseline]}") s"
echo "logical schema dump:                 $(elapsed "${T[lgc0]}" "${T[lgc_dumped]}") s  (${SCHEMA_DUMP_BYTES} bytes, sha256 ${SCHEMA_DUMP_SHA:0:16}...)"
echo "logical restore (schema):            $(elapsed "${T[lgc_dumped]}" "${T[lgc_restored]}") s"
echo "logical restore -> verified:         $(elapsed "${T[lgc0]}" "${T[lgc_done]}") s  (dump + restore + post-restore + verify + migrate verify)"
echo "snapshot restore (CREATE..TEMPLATE): $(elapsed "${T[rst0]}" "${T[rst_cloned]}") s"
echo "snapshot restore -> verified:        $(elapsed "${T[rst0]}" "${T[rst_done]}") s  (restore + post-restore + all checks + migrate verify) (wall time of this local mechanics sequence on this small synthetic dataset; NOT an RTO)"
echo "snapshot RPO on this run:            ${LOST} committed ledger entries written after the snapshot were absent from the restore"
N_PASS=0; N_SNAPCMP=0
for r in "${RESULTS[@]}"; do
  case "$r" in *"|PASS|"*) N_PASS=$((N_PASS + 1)) ;; esac
  case "$r" in snapshot:fingerprint:*"|PASS|"*|snapshot:row_counts_all_tables_all_scopes*"|PASS|"*) N_SNAPCMP=$((N_SNAPCMP + 1)) ;; esac
done
echo "gating checks passed: ${N_PASS} (failed: ${FAILS}); ${N_SNAPCMP} of them compare the file-level clone with its own source and pass by construction"
echo "total drill wall time:               $(elapsed "${T[start]}" "${T[end]}") s"
echo
if [ "$FAILS" -eq 0 ]; then
  echo "DRILL RESULT: PASS (all gating checks passed; the REFUSED row is a finding, see the runbook)"
  exit 0
fi
echo "DRILL RESULT: FAIL (${FAILS} gating check(s) failed)"
exit 1
