#!/usr/bin/env bash
# Mode backup-restore-drill (STAGING-PREP-3, closes gap G1; plan sections 5, 8 and 7.2). SOURCED.
#
# ONE task run that exercises, on the real RDS instance, with the deploy/backup scripts exactly as the plan uses them:
#   5.1/5.2  provision igaming_backup as the master (init-backup-role.sql, BACKUP_ROLE_IAM_ONLY=1, then GRANT rds_iam), pgaudit role setting
#   5.1      harden-backup-surface.sql (RDS may refuse the revoke on pg_catalog functions: recorded as a FINDING, not a failure)
#   5.3      the 15 write/escalation probes + 8 abuse-primitive probes as igaming_backup over IAM database authentication
#   8.1      backup.sh: IAM token auth, sslmode=verify-full with the RDS CA bundle (SHA-256 logged), age-encrypted, Ed25519-signed
#   8.2      two-phase restore into a NEW database: provision as the master, load as a NON-rds_superuser owner login (BLOCKED if RDS cannot
#            provide one, with the exact refusal), verify.sh, DROP of the restore-check database
#   7.2      the PITR baseline: baseline.sql digests (the "expected.txt" of plan 7.4) and T = now() for restore-db-instance-to-point-in-time
#
# Parameters (all optional, non-secret): DRILL_RESTORE_DB (default igaming_restore_check), SKIP_RESTORE (0|1), PRINT_EXPECTED (0|1: print every
# expected.txt line as EXPECTED| records, ~ a few thousand lines).
#
# DOCUMENTED STAGING DEVIATION: the Ed25519 signing key and the age keypair are generated EPHEMERALLY inside this task's storage and never leave
# it; the verification fingerprint is "pinned" from this run's own log. That proves the mechanics (sign, encrypt, verify, decrypt) but NOT the
# out-of-band pinning of ADR 0122 section 5, and the backup set is destroyed with the task (the drill backs up to prove restorability, it
# produces no durable backup). The signing key lives in its own directory, never in the backup directory.
# Exit: 0 no FAIL; 1 a FAIL; 3 STOP (RDS refused CREATE ROLE ... BYPASSRLS or IAM authentication is unusable: plan 5.1, nothing is worked around).

# shellcheck shell=bash
# shellcheck source=deploy/staging-runner/lib-probes.sh
. "${RN_DIR}/lib-probes.sh"

RN_DRILL_DB=""
RN_DRILL_DB_CREATED=0
RN_LOADER_CREATED=0

rn_drill_cleanup() {
  # best effort, never fails the run: drop what THIS run created (by validated name)
  if [ "$RN_DRILL_DB_CREATED" = 1 ] && [ -n "$RN_DRILL_DB" ]; then
    if rn_psql master "$DB_NAME" -c "DROP DATABASE IF EXISTS \"${RN_DRILL_DB}\" WITH (FORCE)" >/dev/null 2>"${RN_SCRATCH}/drop.err"; then
      rn_result restore_check_database_dropped PASS "DROP DATABASE ${RN_DRILL_DB} (exit cleanup)"
    else
      rn_result restore_check_database_dropped FAIL "could not drop ${RN_DRILL_DB}: $(rn_first_line "${RN_SCRATCH}/drop.err") -- drop it by hand"
    fi
    RN_DRILL_DB_CREATED=0
  fi
  if [ "$RN_LOADER_CREATED" = 1 ]; then
    rn_psql master "$DB_NAME" -c "DROP ROLE IF EXISTS igaming_restore_loader" >/dev/null 2>&1 || rn_result restore_loader_role_dropped FAIL "could not drop igaming_restore_loader -- drop it by hand"
    RN_LOADER_CREATED=0
  fi
}

mode_backup_restore_drill() {
  rn_need_tools psql pg_dump pg_restore pg_dumpall openssl age age-keygen sha256sum awk
  local restore_db skip_restore print_expected
  restore_db="$(rn_param DRILL_RESTORE_DB '^[a-z][a-z0-9_]{2,40}$' igaming_restore_check)"
  skip_restore="$(rn_param SKIP_RESTORE '^[01]$' 0)"
  print_expected="$(rn_param PRINT_EXPECTED '^[01]$' 0)"
  case "$restore_db" in "$DB_NAME"|postgres|template0|template1|rdsadmin) rn_die "DRILL_RESTORE_DB must be a new scratch database name" ;; esac
  rn_load_credentials
  [ "$RN_LOCAL" = 1 ] || command -v aws >/dev/null 2>&1 || rn_die "the aws CLI is required to mint the IAM database authentication token"
  rn_fetch_ca
  rn_ensure_tmp
  RN_DRILL_DB="$restore_db"
  rn_on_exit rn_drill_cleanup

  local ossl_major
  ossl_major="$(openssl version | sed -n 's/^OpenSSL \([0-9][0-9]*\)\..*/\1/p')"
  [ "${ossl_major:-0}" -ge 3 ] || rn_die "OpenSSL >= 3.0 is required for Ed25519 signing"
  rn_result tools INFO "$(openssl version); $(age --version 2>&1 | head -1); psql $(psql --version | awk '{print $3}'); pg_dump $(pg_dump --version | awk '{print $3}'); aws $(if command -v aws >/dev/null 2>&1; then aws --version 2>&1 | awk '{print $1}'; else echo n/a; fi)"

  # ------------------------------------------------------------ 0. who am I, what is the source
  local who anchor
  if ! who="$(rn_psql master "$DB_NAME" -F ' ' -c "SELECT current_user, current_setting('server_version'), COALESCE((SELECT pg_has_role(current_user, 'rds_superuser', 'MEMBER')::text FROM pg_roles WHERE rolname = 'rds_superuser'), 'no-rds_superuser-role')" 2>"${RN_SCRATCH}/who.err")"; then
    rn_stop "cannot connect as the master ${DB_MASTER_USER}: $(rn_first_line "${RN_SCRATCH}/who.err")"
  fi
  rn_result master_connection PASS "user, server version, rds_superuser member: ${who}"
  anchor="$(rn_psql owner "$DB_NAME" -c "SELECT COALESCE((SELECT environment FROM database_environment LIMIT 1), 'unrecorded')")"
  rn_result source_environment_anchor INFO "database_environment=${anchor} (restore.sh requires target environment = source environment; a restored copy's anchor is BLANK by design)"

  # a leftover scratch database of an earlier failed drill (same designated name) is removed first
  rn_psql master "$DB_NAME" -c "DROP DATABASE IF EXISTS \"${restore_db}\" WITH (FORCE)" >/dev/null 2>&1 || true

  # ------------------------------------------------------------ 1. provision igaming_backup (plan 5.1)
  local prov_err="${RN_SCRATCH}/provision.err" state
  if [ "$RN_LOCAL" = 1 ]; then
    # local harness only: no rds_iam, so a client-side SCRAM verifier + password file replaces IAM authentication (same code path in backup.sh)
    local pw verifier
    pw="$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32)"
    rn_register_secret "$pw"
    verifier="$(printf '%s' "$pw" | "${RN_BIN}/scramhash")"
    printf '%s' "$pw" >"$RUNNER_BACKUP_PASSWORD_FILE"; chmod 600 "$RUNNER_BACKUP_PASSWORD_FILE"
    IGAMING_BACKUP_PASSWORD_VERIFIER="$verifier" BACKUP_ROLE_VALID_UNTIL="$(date -u -d '+3 hours' '+%Y-%m-%d %H:%M:%S+00')" \
      rn_psql master "$DB_NAME" -f "${RN_BACKUP_DIR}/init-backup-role.sql" >/dev/null 2>"$prov_err" && state=ok || state=refused
  else
    BACKUP_ROLE_IAM_ONLY=1 rn_psql master "$DB_NAME" -f "${RN_BACKUP_DIR}/init-backup-role.sql" >/dev/null 2>"$prov_err" && state=ok || state=refused
  fi
  if [ "$state" != ok ]; then
    rn_result backup_role_provision FINDING "RDS REFUSED (or the script failed) creating igaming_backup (NOSUPERUSER BYPASSRLS ...): $(rn_first_line "$prov_err")"
    rn_stop "plan 5.1: the backup role cannot be created, so ADR 0122 section 3 has no logical-backup role on this instance (PITR remains the primary path); nothing was worked around"
  fi
  rn_result backup_role_provision PASS "igaming_backup created/refreshed by init-backup-role.sql (BACKUP_ROLE_IAM_ONLY=$((1 - RN_LOCAL)))"
  if [ "$RN_LOCAL" = 0 ]; then
    if rn_psql master "$DB_NAME" -c "GRANT rds_iam TO igaming_backup" >/dev/null 2>"${RN_SCRATCH}/iam.err"; then
      rn_result backup_role_rds_iam PASS "GRANT rds_iam TO igaming_backup"
    else
      rn_result backup_role_rds_iam FINDING "GRANT rds_iam refused: $(rn_first_line "${RN_SCRATCH}/iam.err")"
      rn_stop "plan 4.2: IAM database authentication is the required credential mechanism; without rds_iam the role has no usable credential"
    fi
  fi
  # pgaudit role setting (plan 5.4), only when pgaudit is loaded
  if [ "$(rn_psql master "$DB_NAME" -c "SELECT count(*) FROM pg_settings WHERE name = 'pgaudit.log'")" = 1 ]; then
    if rn_psql master "$DB_NAME" -c "ALTER ROLE igaming_backup SET pgaudit.log = 'read, write, ddl, role'" >/dev/null 2>"${RN_SCRATCH}/pga.err"; then
      rn_result backup_role_pgaudit PASS "ALTER ROLE igaming_backup SET pgaudit.log = 'read, write, ddl, role' (a dump now produces AUDIT: lines in /aws/rds/instance/<id>/postgresql)"
    else
      rn_result backup_role_pgaudit FINDING "could not set pgaudit.log for the role: $(rn_first_line "${RN_SCRATCH}/pga.err")"
    fi
  else
    rn_result backup_role_pgaudit INFO "pgaudit is not loaded (db_enable_pgaudit=false or the instance was not rebooted): ADR 0122 falls back to connection logging"
  fi

  # first IAM login: the whole backup path depends on it
  if ! rn_psql backup "$DB_NAME" -c "SELECT 1" >/dev/null 2>"${RN_SCRATCH}/login.err"; then
    rn_result backup_role_login FINDING "the backup role cannot log in with its IAM token: $(rn_first_line "${RN_SCRATCH}/login.err")"
    rn_stop "plan 4.2/8.0: IAM database authentication end to end is unusable (typical causes: the boundary or task-role policy lacks rds-db:connect, iam_database_authentication_enabled is off, the DbiResourceId differs, TLS verification failed); nothing was worked around"
  fi
  rn_result backup_role_login PASS "igaming_backup logs in with a short-lived IAM token over sslmode=${RN_SSLMODE}"

  # ------------------------------------------------------------ 2. harden-backup-surface.sql (plan 5.1)
  local hardening=applied
  if rn_psql master "$DB_NAME" -f "${RN_BACKUP_DIR}/harden-backup-surface.sql" >/dev/null 2>"${RN_SCRATCH}/harden.err"; then
    rn_result harden_backup_surface PASS "harden-backup-surface.sql applied (advisory locks, WAL messages, large objects, NOTIFY revoked from PUBLIC; CONNECT on postgres/template1 revoked from PUBLIC)"
  else
    hardening=refused
    rn_result harden_backup_surface FINDING "RDS REFUSED part of harden-backup-surface.sql (plan G8: revoke on pg_catalog functions): $(rn_first_line "${RN_SCRATCH}/harden.err")"
    local db_
    for db_ in postgres template1; do
      if rn_psql master "$DB_NAME" -c "REVOKE CONNECT ON DATABASE ${db_} FROM PUBLIC" >/dev/null 2>"${RN_SCRATCH}/harden2.err"; then
        rn_result "harden_revoke_connect_${db_}" PASS "REVOKE CONNECT ON DATABASE ${db_} FROM PUBLIC"
      else
        rn_result "harden_revoke_connect_${db_}" FINDING "refused: $(rn_first_line "${RN_SCRATCH}/harden2.err")"
      fi
    done
  fi
  if rn_psql master "$DB_NAME" -c "GRANT CONNECT ON DATABASE postgres TO \"${DB_MASTER_USER}\"" >/dev/null 2>"${RN_SCRATCH}/harden3.err"; then
    rn_result harden_grant_connect_postgres_to_master PASS "operator role keeps CONNECT on postgres"
  else
    rn_result harden_grant_connect_postgres_to_master FINDING "GRANT CONNECT ON DATABASE postgres refused: $(rn_first_line "${RN_SCRATCH}/harden3.err") (the restore scripts use the application database as their maintenance connection)"
  fi

  # ------------------------------------------------------------ 3. the denial probes (plan 5.3)
  rn_backup_role_attributes
  rn_probe_pack "$hardening"

  # ------------------------------------------------------------ 4. ephemeral keys (documented staging deviation)
  local keydir="${RN_SCRATCH}/keys" fp recipient
  mkdir -m 700 "$keydir"
  openssl genpkey -algorithm ED25519 -out "${keydir}/sign.pem" 2>/dev/null || rn_stop "cannot generate an Ed25519 key (OpenSSL >= 3.0 required)"
  chmod 600 "${keydir}/sign.pem"
  openssl pkey -in "${keydir}/sign.pem" -pubout -out "${keydir}/sign.pub" 2>/dev/null
  fp="$(openssl pkey -pubin -in "${keydir}/sign.pub" -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)"
  age-keygen -o "${keydir}/age.key" >/dev/null 2>&1 || rn_stop "cannot generate an age keypair"
  chmod 600 "${keydir}/age.key"
  recipient="$(age-keygen -y "${keydir}/age.key")"
  rn_result signing_key_fingerprint PASS "ephemeral Ed25519 key generated in the task's storage (never leaves the task); verification fingerprint sha256=${fp} pinned from THIS run's log; age recipient ${recipient:0:12}...; keys are outside the backup directory"
  printf 'KEY|signing_fingerprint_sha256|%s\n' "$fp"

  # ------------------------------------------------------------ 5. backup (plan 8.1)
  local outdir="${RN_SCRATCH}/backups" rundir
  mkdir -m 700 "$outdir"
  local -a benv=()
  mapfile -t benv < <(rn_backup_env "$DB_NAME")
  benv+=("BACKUP_SIGNING_KEY_FILE=${keydir}/sign.pem" "BACKUP_ENCRYPT_AGE_RECIPIENT=${recipient}")
  if rn_script_run backup "${RN_SCRATCH}/backup.log" benv backup.sh --out-dir "$outdir" --label drill; then
    rundir="$(find "$outdir" -mindepth 1 -maxdepth 1 -type d | head -1)"
    rn_show_script_output "${RN_SCRATCH}/backup.log"
    local names enc sums_sha
    names="$(find "$rundir" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort | tr '\n' ' ')"
    enc="$(sed -n 's/.*"encryption": "\([a-z]*\)".*/\1/p' "${rundir}/manifest.json" | head -1)"
    sums_sha="$(sha256sum <"${rundir}/SHA256SUMS" | cut -d' ' -f1)"
    if [ "$enc" = age ] && [ -s "${rundir}/SHA256SUMS.sig" ] && [ ! -e "${rundir}/db.dump" ] && [ ! -e "${rundir}/baseline.txt" ] && [ ! -e "${rundir}/globals.sql" ]; then
      rn_result backup_set PASS "manifest encryption=age, signed (SHA256SUMS.sig), no plaintext artefact; files: ${names}"
    else
      rn_result backup_set FAIL "unexpected set: encryption='${enc}', files: ${names}"
    fi
    rn_result backup_sums_sha256 INFO "RESTORE_EXPECT_SET_SHA256 for this set: ${sums_sha}"
    printf 'KEY|set_sha256sums|%s\n' "$sums_sha"
  else
    rn_show_script_output "${RN_SCRATCH}/backup.log"
    rn_result backup FAIL "backup.sh exit $(cat "${RN_SCRATCH}/backup.log.rc" 2>/dev/null || echo '?'): $(grep -E '^backup:' "${RN_SCRATCH}/backup.log" | tail -2 | tr '\n' ' ')"
    rundir=""
  fi

  # ------------------------------------------------------------ 6. two-phase restore + verify (plan 8.2)
  if [ -z "$rundir" ]; then
    rn_result restore BLOCKED "no backup set to restore"
  elif [ "$skip_restore" = 1 ]; then
    rn_result restore INFO "SKIP_RESTORE=1: restore, verify and drop skipped"
  else
    rn_drill_restore "$rundir" "$restore_db" "$anchor" "$sums_sha" "$keydir" "$fp"
  fi

  # ------------------------------------------------------------ 7. the PITR baseline (plan 7.2)
  rn_drill_pitr_baseline "$print_expected"
}

rn_drill_restore() {
  local rundir="$1" db="$2" anchor="$3" sums_sha="$4" keydir="$5" fp="$6"
  local -a renv=()
  local base_env=("RESTORE_CONFIRM=${db}" "RESTORE_EXPECT_SOURCE_ENVIRONMENT=${anchor}" "RESTORE_TARGET_ENVIRONMENT=${anchor}" "RESTORE_EXPECT_SET_SHA256=${sums_sha}"
                  "RESTORE_VERIFY_PUBKEY_FILE=${keydir}/sign.pub" "RESTORE_VERIFY_PUBKEY_SHA256=${fp}" "RESTORE_AGE_IDENTITY_FILE=${keydir}/age.key")
  # phase 1: provision as the master (roles exist; creates only the empty database)
  mapfile -t renv < <(rn_backup_env_passfile "$DB_NAME" "$DB_MASTER_USER" "$RN_PGPASS")
  renv+=("${base_env[@]}")
  RN_DRILL_DB_CREATED=1
  if rn_script_run restore_provision "${RN_SCRATCH}/restore1.log" renv restore.sh --backup-dir "$rundir" --new-database "$db" --phase provision; then
    rn_show_script_output "${RN_SCRATCH}/restore1.log"
    rn_result restore_provision PASS "empty database ${db} created as ${DB_MASTER_USER} (owner igaming); nothing from the archive ran"
  else
    rn_show_script_output "${RN_SCRATCH}/restore1.log"
    RN_DRILL_DB_CREATED=0
    local pmsg
    pmsg="$(grep -E '^(backup|restore):' "${RN_SCRATCH}/restore1.log" | grep -vE 'FAILED \(exit' | tail -1)"
    if grep -qE 'owner role .* holds|refusing to make it the owner' "${RN_SCRATCH}/restore1.log"; then
      # restore.sh requires the owner role (igaming) to be unprivileged; on RDS the master/owner is a member of rds_superuser and holds CREATEROLE
      rn_result restore_provision BLOCKED "restore.sh refused the owner role (plan 8.2, G8: the RDS master igaming is an rds_superuser member): ${pmsg}"
    else
      rn_result restore_provision FAIL "restore.sh --phase provision failed: ${pmsg}"
    fi
    rn_drill_verify_source_only "$rundir" "$keydir" "$anchor"
    return 0
  fi

  # the non-superuser owner login for phase 2
  local lpw verifier until
  lpw="$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32)"
  rn_register_secret "$lpw"
  verifier="$(printf '%s' "$lpw" | "${RN_BIN}/scramhash")"
  until="$(date -u -d '+2 hours' '+%Y-%m-%d %H:%M:%S+00')"
  RN_LOADER_CREATED=1
  if RN_LOADER_VERIFIER="$verifier" rn_psql master "$DB_NAME" -v "owner_role=${DB_OWNER_USER}" -v "valid_until=${until}" -f "${RN_DIR}/sql/create-restore-loader.sql" >/dev/null 2>"${RN_SCRATCH}/loader.err"; then
    RN_LOADER_USER=igaming_restore_loader
    RN_LOADER_PGPASS="${RN_SHM}/loader.pgpass"
    (umask 077; rn_pgpass_line "$RN_HOST" "$RDS_PORT" "$RN_LOADER_USER" "$lpw" >"$RN_LOADER_PGPASS")
    rn_result restore_loader_role PASS "NOSUPERUSER login igaming_restore_loader created as a member of ${DB_OWNER_USER} (valid 2 hours; dropped at exit)"
  else
    RN_LOADER_CREATED=0
    rn_result restore_loader_role BLOCKED "RDS cannot provide a separate NOSUPERUSER owner login: $(rn_first_line "${RN_SCRATCH}/loader.err")"
    rn_drill_verify_source_only "$rundir" "$keydir" "$anchor"
    return 0
  fi

  # phase 2: load as the loader
  mapfile -t renv < <(rn_backup_env_passfile "$DB_NAME" "$RN_LOADER_USER" "$RN_LOADER_PGPASS")
  renv+=("${base_env[@]}")
  if rn_script_run restore_load "${RN_SCRATCH}/restore2.log" renv restore.sh --backup-dir "$rundir" --new-database "$db" --phase load; then
    rn_show_script_output "${RN_SCRATCH}/restore2.log"
    rn_result restore_load PASS "restore.sh --phase load as ${RN_LOADER_USER}: signature verified against the pinned key, checksums, decrypt, single-transaction pg_restore, post-restore grants"
  else
    rn_show_script_output "${RN_SCRATCH}/restore2.log"
    local msg
    msg="$(grep -E '^(backup|restore):' "${RN_SCRATCH}/restore2.log" | grep -vE 'FAILED \(exit' | tail -1)"
    if grep -qE 'refusing to run the archive as' "${RN_SCRATCH}/restore2.log"; then
      rn_result restore_load BLOCKED "restore.sh refused the data-load identity (plan 8.2, G8): ${msg}"
    else
      rn_result restore_load FAIL "restore.sh --phase load failed: ${msg}"
    fi
    RN_DRILL_DB_CREATED=1   # still ours: dropped at the end
    rn_drill_verify_source_only "$rundir" "$keydir" "$anchor"
    return 0
  fi

  # verify.sh against the restored database
  local baseline="${RN_SCRATCH}/baseline.decrypted.txt"
  age -d -i "${keydir}/age.key" -o "$baseline" "${rundir}/baseline.txt.age" 2>/dev/null || { rn_result verify FAIL "cannot decrypt the backup's baseline"; return 0; }
  local -a venv=()
  rn_backup_token_fresh   # verify.sh takes a passfile (not the IAM command): a fresh ~15-minute token file, valid for the whole run
  mapfile -t venv < <(rn_backup_env_passfile "$db" "$RN_BACKUP_USER" "$RN_PGPASS_BACKUP")
  venv+=("VERIFY_OWNER_PGUSER=${DB_OWNER_USER}" "VERIFY_OWNER_PGPASSFILE=${RN_PGPASS}" "VERIFY_RUNTIME_PGPASSFILE=${RN_PGPASS}"
         "VERIFY_OWNER_DATABASE_URL=$(rn_migrate_url owner | sed "s#/${DB_NAME}?#/${db}?#")" "VERIFY_RUNTIME_DATABASE_URL=$(rn_migrate_url runtime | sed "s#/${DB_NAME}?#/${db}?#")"
         "MIGRATE_BIN=${RN_BIN}/migrate" "RESTORECHECK_BIN=${RN_BIN}/restorecheck")
  if rn_script_run verify "${RN_SCRATCH}/verify.log" venv verify.sh --baseline "$baseline" --seq-mode exact --expect-anchor unrecorded --anchor-mode reset; then
    rn_result verify_restored PASS "$(grep 'VERIFY RESULT' "${RN_SCRATCH}/verify.log" | tail -1)"
  else
    rn_result verify_restored FAIL "$(grep 'VERIFY RESULT' "${RN_SCRATCH}/verify.log" | tail -1)"
  fi
  rn_relay_chk "${RN_SCRATCH}/verify.log" verify

  # DROP of the restore-check database (always)
  if rn_psql master "$DB_NAME" -c "DROP DATABASE \"${db}\" WITH (FORCE)" >/dev/null 2>"${RN_SCRATCH}/drop.err"; then
    RN_DRILL_DB_CREATED=0
    rn_result restore_check_database_dropped PASS "DROP DATABASE ${db}"
  else
    rn_result restore_check_database_dropped FAIL "$(rn_first_line "${RN_SCRATCH}/drop.err")"
  fi
}

# rn_drill_verify_source_only : the restore could not complete (BLOCKED / FAIL): verify what is still possible. The backup's decrypted
# baseline is compared with the LIVE source by verify.sh (anchor preserved, sequences >=). The source may have changed since the snapshot
# (the hourly reconciliation writes rows), so these checks are INFORMATION about the verifier pipeline on RDS, never a verdict.
rn_drill_verify_source_only() {
  local rundir="$1" keydir="$2" anchor="$3" baseline="${RN_SCRATCH}/baseline.decrypted.txt"
  age -d -i "${keydir}/age.key" -o "$baseline" "${rundir}/baseline.txt.age" 2>/dev/null || { rn_result source_verify BLOCKED "cannot decrypt the backup's baseline"; return 0; }
  local -a venv=()
  rn_backup_token_fresh
  mapfile -t venv < <(rn_backup_env_passfile "$DB_NAME" "$RN_BACKUP_USER" "$RN_PGPASS_BACKUP")
  venv+=("VERIFY_OWNER_PGUSER=${DB_OWNER_USER}" "VERIFY_OWNER_PGPASSFILE=${RN_PGPASS}" "VERIFY_RUNTIME_PGPASSFILE=${RN_PGPASS}"
         "VERIFY_OWNER_DATABASE_URL=$(rn_migrate_url owner)" "VERIFY_RUNTIME_DATABASE_URL=$(rn_migrate_url runtime)"
         "MIGRATE_BIN=${RN_BIN}/migrate" "RESTORECHECK_BIN=${RN_BIN}/restorecheck")
  local rc=0
  rn_script_run verify_source "${RN_SCRATCH}/verify-source.log" venv verify.sh --baseline "$baseline" --seq-mode ge --expect-anchor "$anchor" --anchor-mode preserved || rc=$?
  RN_CHK_INFO_ONLY=1 rn_relay_chk "${RN_SCRATCH}/verify-source.log" source_verify
  rn_result source_verify INFO "restore did not complete; verify.sh compared the backup baseline with the live source instead (exit ${rc}): $(grep 'VERIFY RESULT' "${RN_SCRATCH}/verify-source.log" | tail -1)"
}

# rn_drill_pitr_baseline : baseline.sql on the SOURCE as the backup role in ONE repeatable-read snapshot, T = that transaction's now().
rn_drill_pitr_baseline() {
  local print_expected="$1" wrapper="${RN_SCRATCH}/pitr-baseline.sql" raw="${RN_SCRATCH}/pitr-baseline.raw" expected="${RN_SCRATCH}/expected.txt" t_utc kind digests="" inv_bad anchor
  cat >"$wrapper" <<EOF
\\set ON_ERROR_STOP on
BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT 'PITRT|' || to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"');
\\i ${RN_BACKUP_DIR}/baseline.sql
COMMIT;
EOF
  if ! rn_psql backup "$DB_NAME" -f "$wrapper" >"$raw" 2>"${RN_SCRATCH}/pitr-baseline.err"; then
    rn_result pitr_baseline FAIL "baseline.sql could not run as igaming_backup: $(rn_first_line "${RN_SCRATCH}/pitr-baseline.err")"
    return 0
  fi
  t_utc="$(sed -n 's/^PITRT|//p' "$raw" | head -1)"
  grep -v '^PITRT|' "$raw" >"$expected"
  for kind in TABLE ROLE MEMBER DBACL SCHEMAACL ACL COLACL FUNCACL DEFACL TRG POL RLS FP; do
    digests="${digests}${digests:+;}${kind}=$({ grep "^${kind}|" "$expected" || true; } | LC_ALL=C sort | sha256sum | cut -c1-16)"
  done
  inv_bad="$(awk -F'|' '$1=="INV" && $3 != "0"' "$expected" | tr '\n' ' ')"
  anchor="$(sed -n 's/^META|environment_anchor|//p' "$expected" | head -1)"
  if [ -z "$inv_bad" ]; then rn_result pitr_baseline_invariants PASS "$(grep -c '^INV|' "$expected") invariant queries, all 0 on the source snapshot"
  else rn_result pitr_baseline_invariants FAIL "non-zero invariants on the source: ${inv_bad}"; fi
  rn_result pitr_baseline PASS "baseline.sql evaluated as igaming_backup in one REPEATABLE READ snapshot: $(grep -c . "$expected") lines, $(grep -c '^TABLE|' "$expected") tables; head $(sed -n 's/^META|migration_head|//p' "$expected" | head -1)"
  printf 'PITR|T_utc=%s\n' "$t_utc"
  printf 'PITR|anchor=%s\n' "$anchor"
  printf 'PITR|expected_digests=%s\n' "$digests"
  printf 'PITR|expected_sha256=%s\n' "$(sha256sum <"$expected" | cut -d' ' -f1)"
  rn_result pitr_parameters INFO "restore-db-instance-to-point-in-time --restore-time ${t_utc} ; then: deploy.sh runner pitr-verify PITR_ENDPOINT=<clone endpoint> EXPECTED_DIGESTS='${digests}' EXPECT_ANCHOR=${anchor} (write nothing to the source between this record and the restore point)"
  if [ "$print_expected" = 1 ]; then
    while IFS= read -r kind; do printf 'EXPECTED|%s\n' "$(rn_wire "$kind")"; done <"$expected"
  fi
}
