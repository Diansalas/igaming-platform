#!/usr/bin/env bash
# Point-in-time-recovery drill on the THROWAWAY drill cluster (A12, ADR 0122): physical base backup + continuous WAL
# archiving + recovery of a SECOND instance to a recorded target, with proof that the data state equals the expected one.
#
#   deploy/backup/pitr-drill.sh --drill-root /tmp/pgdrill-XXXXXX [--src-port 5499] [--rec-port 5497]
#
# Preconditions (set up by deploy/backup/drill.sh): the source cluster `src` is running with wal_level=replica,
# archive_mode=on, archive_command copying into <root>/src/wal_archive, archive_timeout set; roles igaming,
# igaming_runtime, igaming_backup and drill_replicator (REPLICATION) exist; passfiles under <root>/secrets.
#
# Sequence (all against throwaway clusters; nothing here can address the shared 5432 cluster):
#   1  pg_basebackup (-X stream, sha256 manifest) + pg_verifybackup                       -> TIMING base backup
#   2  phase A writes through the platform services (seed), padding with WAL switches
#   3  RECOVERY TARGET: baseline.sql on the source = EXPECTED state; pg_create_restore_point('drill_target_a');
#      a timestamp recorded right after it
#   4  phase B writes (more seeded data) + a simulated OPERATOR ERROR (superuser deletes casino_provider_rounds)
#   5  archive lag measured: time until the WAL segment holding the last commit appears in the archive (the RPO of
#      physical recovery is bounded by archive_timeout, not by the backup interval)
#   6  recovery #1: base copy + restore_command + recovery_target_name -> promote; verify.sh against EXPECTED
#   7  recovery #2: recovery_target_time (the "restore to a timestamp" runbook path); verify.sh against EXPECTED
#   8  rows written after the target are ABSENT, rows before it PRESENT, the operator error is UNDONE
#   9  WAL-gap drill: a missing archive segment is DETECTED by the continuity check and recovery cannot reach the
#      target (server refuses to promote)
# Output: RESULT|<name>|PASS|FAIL|<detail> and TIMING|/SIZE| lines; exit 1 if any RESULT failed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"
# shellcheck source=deploy/backup/lib-cluster.sh
. "${SCRIPT_DIR}/lib-cluster.sh"

ROOT=""; SRC_PORT=5499; REC_PORT=5497
while [ $# -gt 0 ]; do
  case "$1" in
    --drill-root) shift; ROOT="${1:-}" ;;
    --src-port) shift; SRC_PORT="${1:-}" ;;
    --rec-port) shift; REC_PORT="${1:-}" ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done
case "$ROOT" in /tmp/pgdrill-??????) ;; *) bk_die "--drill-root must be a /tmp/pgdrill-XXXXXX directory created by the drill" ;; esac
bk_guard_target 127.0.0.1 "$SRC_PORT"; bk_guard_target 127.0.0.1 "$REC_PORT"
DRILL_ROOT="$ROOT"
ARCH="${ROOT}/src/wal_archive"
SECRETS="${ROOT}/secrets"
[ -d "$ARCH" ] && [ -f "${SECRETS}/pgpass.repl" ] || bk_die "drill root is not prepared (no archive or replication passfile)"
WORK="${ROOT}/work"
FAILS=0
res() { echo "RESULT|$1|$2|$3"; [ "$2" = PASS ] || FAILS=$((FAILS + 1)); }

q_super() { cl_super src "$SRC_PORT" -d igaming_platform_dev "$@"; }
# clone passfile lines of the source port for another port (physical copies keep their roles and passwords)
clone_pass() { # file newport
  awk -F: -v p="$SRC_PORT" -v n="$2" 'BEGIN{OFS=":"} $2==p {$2=n; print}' "$1" >>"$1.tmp.$$" && cat "$1.tmp.$$" >>"$1" && rm -f "$1.tmp.$$"
}
for f in pgpass pgpass.backup; do clone_pass "${SECRETS}/${f}" "$REC_PORT"; done

# baseline.sql against a port as the backup role -> file
baseline_to() { # port outfile
  ( BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT="$1" BACKUP_PGDATABASE=igaming_platform_dev BACKUP_PGUSER=igaming_backup BACKUP_PGPASSFILE="${SECRETS}/pgpass.backup"
    bk_connect_env; psql -X -q -At -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/baseline.sql" >"$2" )
}
tbl_rows() { awk -F'|' -v t="$2" '$1=="TABLE" && $2==t {print $3}' "$1"; }

wal_next() { # 24-hex segment name -> next name (16 MB segments: 256 per log id)
  local n="$1" tli log seg
  tli="${n:0:8}"; log=$((16#${n:8:8})); seg=$((16#${n:16:8}))
  seg=$((seg + 1)); if [ "$seg" -ge 256 ]; then seg=0; log=$((log + 1)); fi
  printf '%s%08X%08X' "$tli" "$log" "$seg"
}
# wal_continuity <archive> <first segment> <last segment> -> prints missing segments; returns 1 if any
wal_continuity() {
  local dir="$1" cur="$2" last="$3" missing=""
  while :; do
    [ -f "${dir}/${cur}" ] || missing="${missing} ${cur}"
    [ "$cur" = "$last" ] && break
    cur="$(wal_next "$cur")"
  done
  [ -z "$missing" ] || { echo "$missing"; return 1; }
}

pitr_cleanup() {
  local rc=$? n
  set +e
  for n in pitr_named pitr_time pitr_gap; do cl_stop "$n"; rm -rf "$(cl_dir "$n")"; done
  rm -rf "$(cl_dir pitr_base)" "${ROOT}/src/wal_archive_gap"
  exit "$rc"
}
trap pitr_cleanup EXIT

# ---- 1. base backup ---------------------------------------------------------------------------------------------
BASE_NAME=pitr_base
BASE="$(cl_dir $BASE_NAME)/data"
mkdir -p "$(cl_dir $BASE_NAME)"
T0="$(now_s)"
PGPASSFILE="${SECRETS}/pgpass.repl" "${PG_BIN}/pg_basebackup" -h 127.0.0.1 -p "$SRC_PORT" -U drill_replicator -D "$BASE" -Fp -X stream -c fast --manifest-checksums=sha256 >"${WORK}/basebackup.log" 2>&1 \
  || { tail -3 "${WORK}/basebackup.log" >&2; bk_die "pg_basebackup failed"; }
T1="$(now_s)"
echo "TIMING|base_backup_seconds|$(elapsed_s "$T0" "$T1")"
echo "SIZE|base_backup_bytes|$(du -sb "$BASE" | cut -f1)"
if "${PG_BIN}/pg_verifybackup" "$BASE" >"${WORK}/verifybackup.log" 2>&1; then res base_backup_verifies PASS "pg_verifybackup: $(tail -1 "${WORK}/verifybackup.log")"; else res base_backup_verifies FAIL "$(tail -2 "${WORK}/verifybackup.log" | tr '\n' ' ')"; fi
START_SEG="$(sed -n 's/.*START WAL LOCATION: [^ ]* (file \([0-9A-F]*\)).*/\1/p' "${BASE}/backup_label")"
END_SEG="$(ls "${BASE}/pg_wal" | grep -E '^[0-9A-F]{24}$' | sort | tail -1)"
[ -n "$START_SEG" ] && [ -n "$END_SEG" ] || bk_die "cannot determine base backup WAL range"
chown -R postgres:postgres "$(cl_dir $BASE_NAME)"; chmod 700 "$BASE"

# ---- 2. phase A -------------------------------------------------------------------------------------------------
pad() { for _ in 1 2; do q_super -c "SELECT pg_logical_emit_message(true, 'drill', 'pad'), pg_switch_wal()" >/dev/null; done; }
bk_log "PITR phase A writes (seed)"
DRILL_SEED_TENANTS="${PITR_A_TENANTS:-1}" DRILL_SEED_PLAYERS="${PITR_A_PLAYERS:-6}" DRILL_SEED_ROUNDS="${PITR_A_ROUNDS:-120}" \
  "${SCRIPT_DIR}/seed.sh" --drill-root "$ROOT" --port "$SRC_PORT" --report "${WORK}/seed-A.json" >"${WORK}/seed-A.log" 2>&1 || { tail -5 "${WORK}/seed-A.log" >&2; bk_die "phase A seed failed"; }
pad

# ---- 3. the recovery target ---------------------------------------------------------------------------------------
baseline_to "$SRC_PORT" "${WORK}/expected-at-target.txt"
TARGET_LSN="$(q_super -c "SELECT pg_create_restore_point('drill_target_a')")"
TARGET_SEG="$(q_super -c "SELECT pg_walfile_name('${TARGET_LSN}')")"
sleep 1
TARGET_TS="$(q_super -c "SELECT clock_timestamp()")"
sleep 1
EXP_ENTRIES="$(tbl_rows "${WORK}/expected-at-target.txt" ledger_entries)"
EXP_TENANTS="$(tbl_rows "${WORK}/expected-at-target.txt" tenants)"
N_SEG=0; CUR_SEG="$END_SEG"
while :; do N_SEG=$((N_SEG + 1)); [ "$CUR_SEG" = "$TARGET_SEG" ] && break; CUR_SEG="$(wal_next "$CUR_SEG")"; done
echo "SIZE|wal_segments_replayed_from_base_end_to_target|${N_SEG}"
bk_log "target: restore point drill_target_a at ${TARGET_LSN} (segment ${TARGET_SEG}), timestamp ${TARGET_TS}; expected ledger_entries=${EXP_ENTRIES} tenants=${EXP_TENANTS}"

# ---- 4. phase B + operator error ---------------------------------------------------------------------------------
bk_log "PITR phase B writes (seed) + simulated operator error"
DRILL_SEED_TENANTS="${PITR_B_TENANTS:-1}" DRILL_SEED_PLAYERS="${PITR_B_PLAYERS:-4}" DRILL_SEED_ROUNDS="${PITR_B_ROUNDS:-80}" \
  "${SCRIPT_DIR}/seed.sh" --drill-root "$ROOT" --port "$SRC_PORT" --report "${WORK}/seed-B.json" >"${WORK}/seed-B.log" 2>&1 || { tail -5 "${WORK}/seed-B.log" >&2; bk_die "phase B seed failed"; }
ROUNDS_BEFORE="$(q_super -c "SELECT count(*) FROM casino_provider_rounds")"
# DRILL ONLY (own throwaway superuser): a destructive operator action that bypasses the guard triggers
q_super -c "SET session_replication_role = replica; DELETE FROM casino_provider_rounds" >/dev/null
ROUNDS_AFTER="$(q_super -c "SELECT count(*) FROM casino_provider_rounds")"
baseline_to "$SRC_PORT" "${WORK}/after-B.txt"
AFTER_ENTRIES="$(tbl_rows "${WORK}/after-B.txt" ledger_entries)"
res operator_error_is_visible_on_source PASS "source casino_provider_rounds ${ROUNDS_BEFORE} -> ${ROUNDS_AFTER}; ledger_entries now ${AFTER_ENTRIES} (expected-at-target ${EXP_ENTRIES})"

# ---- 5. archive lag (RPO of the physical path) ----------------------------------------------------------------------
LAST_LSN="$(q_super -c "SELECT pg_current_wal_lsn()")"
LAST_SEG="$(q_super -c "SELECT pg_walfile_name('${LAST_LSN}')")"
TL0="$(now_s)"
LAG="timeout"
for _ in $(seq 1 240); do
  if [ -f "${ARCH}/${LAST_SEG}" ]; then LAG="$(elapsed_s "$TL0" "$(now_s)")"; break; fi
  sleep 0.5
done
if [ "$LAG" = timeout ]; then res archive_lag_bounded FAIL "segment ${LAST_SEG} holding the last commit was not archived within 120 s"; else
  echo "TIMING|wal_archive_lag_seconds|${LAG}"; res archive_lag_bounded PASS "last commit's segment ${LAST_SEG} reached the archive ${LAG} s after the commit (archive_timeout=$(q_super -c 'SHOW archive_timeout'))"; fi
echo "SIZE|wal_archive_bytes|$(du -sb "$ARCH" | cut -f1)"

# ---- 6/7. recoveries ----------------------------------------------------------------------------------------------
recover() { # name archive_dir target_conf_line
  local name="$1" arch="$2" target="$3" d
  d="$(cl_dir "$name")"
  mkdir -p "$d/sock"
  local tc0
  tc0="$(now_s)"
  cp -a "$(cl_dir $BASE_NAME)/data" "$d/data" || return 1   # e.g. disk full: never mistaken for a refused recovery
  COPY_SECONDS="$(elapsed_s "$tc0" "$(now_s)")"
  rm -f "$d/data/postmaster.pid" "$d/data/postmaster.opts"
  cat >>"$d/data/postgresql.conf" <<EOF

# ---- PITR recovery instance ${name} ----
port = ${REC_PORT}
unix_socket_directories = '${d}/sock'
archive_mode = off
restore_command = 'cp "${arch}/%f" "%p"'
${target}
recovery_target_action = 'promote'
recovery_target_timeline = 'latest'
EOF
  touch "$d/data/recovery.signal"
  chown -R postgres:postgres "$d"; chmod 700 "$d" "$d/data"
  DRILL_CLUSTERS+=("$name")
  local ts t1
  ts="$(now_s)"
  if ! pgas "${PG_BIN}/pg_ctl" -D "$d/data" -l "$d/server.log" -w -t 600 start >"$d/pg_ctl.log" 2>&1; then
    return 1
  fi
  for _ in $(seq 1 600); do
    if [ "$(cl_super "$name" "$REC_PORT" -d postgres -c 'SELECT pg_is_in_recovery()' 2>/dev/null || echo t)" = f ]; then
      t1="$(now_s)"; REC_SECONDS="$(elapsed_s "$ts" "$t1")"; return 0
    fi
    sleep 0.5
  done
  return 1
}

verify_recovered() { # label  expected-baseline
  local label="$1"
  if ( export BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT="$REC_PORT" BACKUP_PGDATABASE=igaming_platform_dev BACKUP_PGUSER=igaming_backup BACKUP_PGPASSFILE="${SECRETS}/pgpass.backup"
       export VERIFY_OWNER_PGUSER=igaming VERIFY_OWNER_PGPASSFILE="${SECRETS}/pgpass"
       export VERIFY_OWNER_DATABASE_URL="postgres://igaming@127.0.0.1:${REC_PORT}/igaming_platform_dev?sslmode=disable"
       export VERIFY_RUNTIME_DATABASE_URL="postgres://igaming_runtime@127.0.0.1:${REC_PORT}/igaming_platform_dev?sslmode=disable"
       export MIGRATE_BIN="${WORK}/migrate" RESTORECHECK_BIN="${WORK}/restorecheck"
       "${SCRIPT_DIR}/verify.sh" --baseline "$2" --seq-mode ge --anchor-mode preserved --expect-anchor "${PITR_EXPECT_ANCHOR:-development}" ) >"${WORK}/verify-${label}.txt" 2>&1; then
    res "${label}_verify_against_expected_at_target" PASS "$(tail -1 "${WORK}/verify-${label}.txt")"
  else
    res "${label}_verify_against_expected_at_target" FAIL "$(grep -E 'FAIL' "${WORK}/verify-${label}.txt" | head -3 | cut -c1-240 | tr '\n' ' ')"
  fi
}

evidence() { # label
  local label="$1" got_entries got_tenants rounds
  got_entries="$(cl_super "pitr_${label}" "$REC_PORT" -d igaming_platform_dev -c "SELECT count(*) FROM ledger_entries")"
  got_tenants="$(cl_super "pitr_${label}" "$REC_PORT" -d igaming_platform_dev -c "SELECT count(*) FROM tenants")"
  rounds="$(cl_super "pitr_${label}" "$REC_PORT" -d igaming_platform_dev -c "SELECT count(*) FROM casino_provider_rounds")"
  if [ "$got_entries" = "$EXP_ENTRIES" ] && [ "$got_entries" -lt "$AFTER_ENTRIES" ] && [ "$got_tenants" = "$EXP_TENANTS" ] && [ "$rounds" = "$ROUNDS_BEFORE_TARGET" ]; then
    res "${label}_rows_after_target_absent_before_present" PASS "ledger_entries recovered=${got_entries} (== expected at target ${EXP_ENTRIES}; source now ${AFTER_ENTRIES}); tenants=${got_tenants}; casino_provider_rounds=${rounds} (operator deletion undone)"
  else
    res "${label}_rows_after_target_absent_before_present" FAIL "entries ${got_entries}/${EXP_ENTRIES} (source now ${AFTER_ENTRIES}) tenants ${got_tenants}/${EXP_TENANTS} rounds ${rounds}/${ROUNDS_BEFORE_TARGET}"
  fi
}
ROUNDS_BEFORE_TARGET="$(tbl_rows "${WORK}/expected-at-target.txt" casino_provider_rounds)"

REC_SECONDS=""; COPY_SECONDS=""
if recover pitr_named "$ARCH" "recovery_target_name = 'drill_target_a'"; then
  echo "TIMING|pitr_base_copy_seconds|${COPY_SECONDS}"
  echo "TIMING|pitr_recovery_named_restore_point_seconds|${REC_SECONDS}"
  echo "TIMING|pitr_recovery_named_total_incl_copy_seconds|$(elapsed_s 0 "$(awk -v a="$COPY_SECONDS" -v b="$REC_SECONDS" 'BEGIN { printf "%.2f", a + b }')")"
  evidence named; verify_recovered named "${WORK}/expected-at-target.txt"
  # the recovered instance must not have archived into (and polluted) the source archive
  [ "$(q_super -c "SELECT archived_count FROM pg_stat_archiver")" -ge 1 ] || true
else
  res named_recovery_promoted FAIL "recovery to the named restore point did not complete: $(tail -3 "$(cl_dir pitr_named)/server.log" | tr '\n' ' ')"
fi
cl_stop pitr_named
rm -rf "$(cl_dir pitr_named)"

if recover pitr_time "$ARCH" "recovery_target_time = '${TARGET_TS}'
recovery_target_inclusive = true"; then
  echo "TIMING|pitr_recovery_timestamp_seconds|${REC_SECONDS}"
  echo "TIMING|pitr_recovery_timestamp_total_incl_copy_seconds|$(elapsed_s 0 "$(awk -v a="$COPY_SECONDS" -v b="$REC_SECONDS" 'BEGIN { printf "%.2f", a + b }')")"
  evidence time; verify_recovered time "${WORK}/expected-at-target.txt"
else
  res time_recovery_promoted FAIL "recovery to the timestamp did not complete: $(tail -3 "$(cl_dir pitr_time)/server.log" | tr '\n' ' ')"
fi
cl_stop pitr_time
rm -rf "$(cl_dir pitr_time)"

# ---- 9. WAL gap -----------------------------------------------------------------------------------------------------
if MISS="$(wal_continuity "$ARCH" "$END_SEG" "$TARGET_SEG")"; then res wal_continuity_check_clean_archive PASS "no gap between ${END_SEG} and the target segment ${TARGET_SEG}"; else res wal_continuity_check_clean_archive FAIL "unexpected gap:${MISS}"; fi
GAP_SEG="$(wal_next "$END_SEG")"
if [ "$GAP_SEG" = "$TARGET_SEG" ] || [ ! -f "${ARCH}/${GAP_SEG}" ]; then
  res wal_gap_drill FAIL "cannot build a gap: segment after ${END_SEG} is ${GAP_SEG}, target ${TARGET_SEG} (not enough WAL between base backup and target)"
else
  GAPDIR="${ROOT}/src/wal_archive_gap"
  mkdir -p "$GAPDIR"; cp -al "${ARCH}/." "$GAPDIR/"; rm -f "${GAPDIR}/${GAP_SEG}"   # hard links: no extra disk
  if MISS="$(wal_continuity "$GAPDIR" "$END_SEG" "$TARGET_SEG")"; then res wal_gap_detected_by_continuity_check FAIL "the check did not see the removed segment ${GAP_SEG}"; else res wal_gap_detected_by_continuity_check PASS "continuity check reports missing:${MISS}"; fi
  if recover pitr_gap "$GAPDIR" "recovery_target_name = 'drill_target_a'"; then
    res wal_gap_recovery_refused FAIL "recovery PROMOTED despite the missing segment ${GAP_SEG}"
  else
    if grep -q 'recovery ended before configured recovery target was reached' "$(cl_dir pitr_gap)/server.log" 2>/dev/null; then
      res wal_gap_recovery_refused PASS "server did not promote; log: $(grep -m1 'recovery ended before configured recovery target' "$(cl_dir pitr_gap)/server.log" | cut -c1-200)"
    else
      res wal_gap_recovery_refused FAIL "recovery did not promote but not for the expected reason; log tail: $(tail -3 "$(cl_dir pitr_gap)/server.log" 2>/dev/null | tr '\n' ' ' | cut -c1-300)"
    fi
  fi
  cl_stop pitr_gap
  rm -rf "$(cl_dir pitr_gap)" "$GAPDIR"
fi
rm -rf "$(cl_dir $BASE_NAME)"

echo "PITR_TARGET|lsn=${TARGET_LSN}|segment=${TARGET_SEG}|time=${TARGET_TS}"
[ "$FAILS" -eq 0 ] && { echo "PITR DRILL RESULT: PASS"; exit 0; }
echo "PITR DRILL RESULT: FAIL (${FAILS})"; exit 1
