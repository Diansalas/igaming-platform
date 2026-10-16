#!/usr/bin/env bash
# Mode pitr-verify (STAGING-PREP-3, plan 7.4). SOURCED.
#
# Verifies a PITR-restored (point-in-time) CLONE of the staging instance against the baseline the backup-restore-drill printed for the restore
# point T (plan 7.2). The operator restored the clone with `aws rds restore-db-instance-to-point-in-time --restore-time <T>` (plan 7.3).
#
# Parameters (non-secret):
#   PITR_ENDPOINT      the clone's endpoint host name (igaming-staging-...<id>.eu-central-1.rds.amazonaws.com); never the primary
#   EXPECTED_DIGESTS   the `PITR|expected_digests=` value of the drill (KIND=hex16;KIND=hex16;...)
#   EXPECT_ANCHOR      the `PITR|anchor=` value (physical recovery PRESERVES the environment anchor; default unrecorded)
# What it does, as the master with the SAME master/runtime credentials as the primary (a clone is restored from the primary's data; if they do
# not work the run STOPs and records that, plan 7.3 "unverified: whether the managed master secret follows"):
#   1. resets the clone's copy of igaming_backup to a short-lived password credential (the task role cannot mint tokens for the clone);
#   2. re-applies harden-backup-surface.sql and the database-level privileges (post-restore-grants.sql: TEMPORARY revoked, CONNECT grants) and checks them;
#   3. evaluates baseline.sql as igaming_backup on the clone and compares the per-kind digests with EXPECTED_DIGESTS (rows written after T must be
#      ABSENT: any such row changes the TABLE digest);
#   4. runs verify.sh on the clone for the structural gates (invariants, drill verify.sql as the owner, migrate verify, restorecheck as the runtime
#      role). DEVIATION from the plan's literal "--baseline expected.txt": a task cannot carry the multi-hundred-KB expected.txt between runs, so verify.sh
#      is given the clone's OWN baseline (its line-by-line comparison is then trivially equal and is not relayed); the real source-vs-clone comparison is
#      step 3. Sequences are therefore not compared with the source (the INV sequence_behind_column_max invariant still is).
# Nothing is written to the primary. The clone is deleted by the operator afterwards (plan 7.5).

# shellcheck shell=bash
# shellcheck source=deploy/staging-runner/lib-probes.sh
. "${RN_DIR}/lib-probes.sh"

mode_pitr_verify() {
  rn_need_tools psql openssl awk sha256sum
  local endpoint digests anchor_expect src_host src_port
  endpoint="$(rn_param PITR_ENDPOINT '^(igaming-staging-db-pitr-[a-z0-9-]{1,50}\.[a-z0-9]{8,20}\.eu-central-1\.rds\.amazonaws\.com|127\.0\.0\.1)$')"
  digests="$(rn_param EXPECTED_DIGESTS '^([A-Z]+=[0-9a-f]{16};)*[A-Z]+=[0-9a-f]{16}$')"
  anchor_expect="$(rn_param EXPECT_ANCHOR '^(unrecorded|staging|development|test|production)$' unrecorded)"
  src_host="${RDS_HOST:-}"; src_port="${RDS_PORT:-}"
  if [ "$endpoint" = 127.0.0.1 ]; then
    # local test harness only: the clone is a second throwaway cluster on another drill port
    [ "${RUNNER_ALLOW_LOCAL:-}" = local-test-harness ] || rn_die "PITR_ENDPOINT must be an RDS endpoint"
    RDS_PORT="$(rn_param PITR_PORT '^549[789]$')"
  fi
  { [ "$endpoint" != "$src_host" ] || [ "${RDS_PORT:-}" != "$src_port" ]; } || rn_die "PITR_ENDPOINT is the primary instance: this mode re-applies grants and resets a credential, and runs only on a disposable clone"
  RDS_HOST="$endpoint"          # every connection of this run goes to the clone (the passfile lines are written for it)
  rn_load_credentials
  rn_fetch_ca
  rn_ensure_tmp

  # ---- 0. the clone
  local info
  if ! info="$(rn_psql master "$DB_NAME" -F ' ' -c "SELECT current_user, current_setting('server_version'), pg_is_in_recovery(), to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'), (SELECT count(*) FROM pg_roles WHERE rolname = 'igaming_backup')" 2>"${RN_SCRATCH}/clone.err")"; then
    rn_result pitr_clone_login FAIL "cannot connect to the clone as the master with the primary's master credential: $(rn_first_line "${RN_SCRATCH}/clone.err")"
    rn_stop "plan 7.3 (unverified): a PITR clone did not accept the primary's master password; record this finding (the managed master secret does not follow the clone); nothing was worked around"
  fi
  rn_result pitr_clone_login PASS "user, version, in_recovery, now, backup role present: ${info}"
  case "$info" in *" f "*) ;; *) rn_result pitr_clone_state FAIL "the clone is still in recovery or unreadable: ${info}" ;; esac

  # ---- 1. credential reset on the clone
  local pw verifier until
  pw="$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32)"
  rn_register_secret "$pw"
  verifier="$(printf '%s' "$pw" | "${RN_BIN}/scramhash")"
  until="$(date -u -d '+2 hours' '+%Y-%m-%d %H:%M:%S+00')"
  if RN_BACKUP_VERIFIER="$verifier" rn_psql master "$DB_NAME" -v "valid_until=${until}" -f "${RN_DIR}/sql/reset-backup-credential.sql" >/dev/null 2>"${RN_SCRATCH}/reset.err"; then
    rn_result pitr_backup_credential_reset PASS "igaming_backup on the clone: rds_iam revoked, short-lived password credential (2 hours) set; the primary was not touched"
  else
    rn_result pitr_backup_credential_reset FINDING "could not reset the clone's backup credential: $(rn_first_line "${RN_SCRATCH}/reset.err")"
    rn_stop "plan 7.4: no usable backup-role credential on the clone; nothing was worked around"
  fi
  RN_BACKUP_STATIC_PASSFILE="${RN_SHM}/clone-backup.pgpass"
  # shellcheck disable=SC2153  # RN_HOST is set by rn_load_credentials
  (umask 077; rn_pgpass_line "$RN_HOST" "$RDS_PORT" "$RN_BACKUP_USER" "$pw" >"$RN_BACKUP_STATIC_PASSFILE")
  if ! rn_psql backup "$DB_NAME" -c "SELECT 1" >/dev/null 2>"${RN_SCRATCH}/clonelogin.err"; then
    rn_result pitr_backup_login FAIL "igaming_backup cannot log in to the clone: $(rn_first_line "${RN_SCRATCH}/clonelogin.err")"
    rn_stop "plan 7.4: igaming_backup cannot log in to the clone"
  fi
  rn_result pitr_backup_login PASS "igaming_backup logs in to the clone with the reset credential"

  # ---- 2. re-apply hardening and the database-level privileges
  local hardening=applied
  if rn_psql master "$DB_NAME" -f "${RN_BACKUP_DIR}/harden-backup-surface.sql" >/dev/null 2>"${RN_SCRATCH}/pharden.err"; then
    rn_result pitr_harden_backup_surface PASS "harden-backup-surface.sql re-applied on the clone"
  else
    hardening=refused
    rn_result pitr_harden_backup_surface FINDING "RDS refused part of harden-backup-surface.sql on the clone: $(rn_first_line "${RN_SCRATCH}/pharden.err")"
  fi
  if rn_psql owner "$DB_NAME" -f "${RN_BACKUP_DIR}/post-restore-grants.sql" >/dev/null 2>"${RN_SCRATCH}/pgrants.err"; then
    rn_result pitr_database_privileges PASS "post-restore-grants.sql re-applied (TEMPORARY revoked from PUBLIC and igaming_runtime, CONNECT grants, schema owner)"
  else
    rn_result pitr_database_privileges FAIL "post-restore-grants.sql failed on the clone: $(rn_first_line "${RN_SCRATCH}/pgrants.err")"
  fi
  rn_psql owner "$DB_NAME" -v owner_role="$DB_OWNER_USER" -v expect_backup=present -v active_kid=unset -v file_head=0 -v file_count=0 -v listing=0 -v require_anchor=0 -v iam_only=0 -f "${RN_DIR}/sql/verify-db.sql" >"${RN_SCRATCH}/pverify-db.out" 2>/dev/null || true
  local checkline
  for checkline in temp_revoked_runtime temp_revoked_public temp_backup_role role_backup_attributes; do
    grep "^RESULT|${checkline}|" "${RN_SCRATCH}/pverify-db.out" | sed "s/^RESULT|/RESULT|pitr_/" | while IFS= read -r l; do
      printf '%s\n' "$(rn_wire "$l")"; printf '%s\n' "$(printf '%s' "$l" | cut -d'|' -f3)" >>"$RN_RESULTS"
    done
  done
  rn_backup_role_attributes
  rn_probe_pack "$hardening"

  # ---- 3. the baseline on the clone and the digest comparison with the source at T
  local raw="${RN_SCRATCH}/clone-baseline.txt" kind pair exp got
  if ! rn_psql backup "$DB_NAME" -f "${RN_BACKUP_DIR}/baseline.sql" >"$raw" 2>"${RN_SCRATCH}/clone-baseline.err"; then
    rn_result pitr_baseline FAIL "baseline.sql could not run on the clone as igaming_backup: $(rn_first_line "${RN_SCRATCH}/clone-baseline.err")"
    return 0
  fi
  for pair in ${digests//;/ }; do
    kind="${pair%%=*}"; exp="${pair#*=}"
    got="$({ grep "^${kind}|" "$raw" || true; } | LC_ALL=C sort | sha256sum | cut -c1-16)"
    if [ "$got" = "$exp" ]; then rn_result "pitr_digest_${kind}" PASS "$(grep -c "^${kind}|" "$raw") lines; digest ${got} equals the source at T"
    else
      rn_result "pitr_digest_${kind}" FAIL "digest ${got} differs from the source at T (${exp}); ${kind} lines on the clone: $(grep -c "^${kind}|" "$raw")"
      { grep "^${kind}|" "$raw" || true; } | LC_ALL=C sort | awk -v n="${PITR_SHOW_LINES:-40}" 'NR <= n' | while IFS= read -r l; do printf 'CLONE|%s\n' "$(rn_wire "$l")"; done
    fi
  done
  local inv_bad
  inv_bad="$(awk -F'|' '$1=="INV" && $3 != "0"' "$raw" | tr '\n' ' ')"
  if [ -z "$inv_bad" ]; then rn_result pitr_invariants PASS "$(grep -c '^INV|' "$raw") invariant queries, all 0 on the clone"; else rn_result pitr_invariants FAIL "non-zero invariants: ${inv_bad}"; fi
  rn_result pitr_clone_anchor INFO "clone environment anchor: $(sed -n 's/^META|environment_anchor|//p' "$raw" | head -1); migration head: $(sed -n 's/^META|migration_head|//p' "$raw" | head -1)"

  # ---- 4. structural gates of verify.sh on the clone
  local -a venv=()
  mapfile -t venv < <(rn_backup_env_passfile "$DB_NAME" "$RN_BACKUP_USER" "$RN_BACKUP_STATIC_PASSFILE")
  venv+=("VERIFY_OWNER_PGUSER=${DB_OWNER_USER}" "VERIFY_OWNER_PGPASSFILE=${RN_PGPASS}" "VERIFY_RUNTIME_PGPASSFILE=${RN_PGPASS}"
         "VERIFY_OWNER_DATABASE_URL=$(rn_migrate_url owner)" "VERIFY_RUNTIME_DATABASE_URL=$(rn_migrate_url runtime)"
         "MIGRATE_BIN=${RN_BIN}/migrate" "RESTORECHECK_BIN=${RN_BIN}/restorecheck")
  local rc=0
  rn_script_run pitr_verify "${RN_SCRATCH}/pverify.log" venv verify.sh --baseline "$raw" --seq-mode ge --expect-anchor "$anchor_expect" --anchor-mode preserved --expect-actor-proof skip || rc=$?
  RN_CHK_SKIP_RE='^(baseline_queries_run|equal_|sequences_not_behind_baseline|baseline_itself_invariant_clean|anchor_blank_after_restore)' rn_relay_chk "${RN_SCRATCH}/pverify.log" pitr_verify
  if [ "$rc" -eq 0 ]; then rn_result pitr_verify_structural PASS "$(grep 'VERIFY RESULT' "${RN_SCRATCH}/pverify.log" | tail -1) (line-by-line baseline gates excluded: see the digest results above)"
  else rn_result pitr_verify_structural FAIL "$(grep 'VERIFY RESULT' "${RN_SCRATCH}/pverify.log" | tail -1)"; fi
}
