#!/usr/bin/env bash
# Mode cleanup (STAGING-PREP-3, SRP3-9; plan 8.4 and 9): remove what an interrupted backup-restore-drill (or any aborted step of the plan) can
# leave behind on the PRIMARY. Idempotent: running it when nothing is left is a PASS. SOURCED.
#
# It removes, as the master:
#   * the disposable restore-loader login igaming_restore_loader (created by the drill's two-phase restore, 2-hour validity);
#   * the restore-check database: every database named igaming_restore* (the drill's default is igaming_restore_check) plus the optional
#     CLEANUP_DB (a custom DRILL_RESTORE_DB), never the application database, postgres, template0/1, rdsadmin;
#   * on RDS only: a password-style state on igaming_backup (finite validity, or no rds_iam membership): it is restored to IAM-only (PASSWORD NULL,
#     VALID UNTIL infinity, GRANT rds_iam), never made "permanent" (SRP3R-2).
# It never touches igaming_backup otherwise (that role stays by design, ADR 0122), data, or any other database: an unknown extra database is
# REPORTED (a FAIL in verify-db) but not dropped. Parameters (non-secret): CLEANUP_DB optional. Run it only when no drill is in progress.
# verify-db's leftover checks (no loader role, no restore-check database, no finite backup credential) are the proof it worked.

# shellcheck shell=bash

mode_cleanup() {
  rn_need_tools psql
  local extra db list="${RN_SCRATCH}/cleanup.list" n=0 left
  extra="$(rn_param CLEANUP_DB '^([a-z][a-z0-9_]{2,40})?$' "")"
  case "$extra" in "$DB_NAME"|postgres|template0|template1|rdsadmin) rn_die "CLEANUP_DB must be a scratch database name, never ${extra}" ;; esac
  rn_load_credentials
  rn_fetch_ca
  # DB_NAME and CLEANUP_DB are validated against [a-z][a-z0-9_]* above / in rn_load_credentials, so they are safe literals here
  if ! rn_psql master "$DB_NAME" -c "SELECT datname FROM pg_database WHERE datname NOT IN ('${DB_NAME}', 'postgres', 'template0', 'template1', 'rdsadmin') AND (datname LIKE 'igaming\_restore%' OR datname = NULLIF('${extra}', '')) ORDER BY 1" >"$list" 2>"${RN_SCRATCH}/cleanup.err"; then
    rn_result cleanup_connect FAIL "cannot connect as the master ${DB_MASTER_USER}: $(rn_first_line "${RN_SCRATCH}/cleanup.err")"
    return 0
  fi
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    [[ "$db" =~ ^[a-z][a-z0-9_]{0,62}$ ]] || { rn_result cleanup_database FAIL "a database with an unexpected name was found and not touched"; continue; }
    if rn_psql master "$DB_NAME" -c "DROP DATABASE IF EXISTS \"${db}\" WITH (FORCE)" >/dev/null 2>"${RN_SCRATCH}/cleanup.err"; then
      n=$((n + 1)); rn_result cleanup_database_dropped PASS "dropped leftover database ${db}"
    else
      rn_result cleanup_database_dropped FAIL "could not drop ${db}: $(rn_first_line "${RN_SCRATCH}/cleanup.err")"
    fi
  done <"$list"
  [ "$n" -gt 0 ] || rn_result cleanup_database_dropped PASS "no restore-check database found (nothing to drop)"

  if [ "$(rn_psql master "$DB_NAME" -c "SELECT count(*) FROM pg_roles WHERE rolname = 'igaming_restore_loader'")" = 1 ]; then
    # a loader that still owns something (it should not: the restore-check database is gone) is reported by the DROP ROLE error
    if rn_psql master "$DB_NAME" -c "DROP ROLE igaming_restore_loader" >/dev/null 2>"${RN_SCRATCH}/cleanup.err"; then
      rn_result cleanup_loader_role PASS "dropped leftover role igaming_restore_loader"
    else
      rn_result cleanup_loader_role FAIL "could not drop igaming_restore_loader: $(rn_first_line "${RN_SCRATCH}/cleanup.err")"
    fi
  else
    rn_result cleanup_loader_role PASS "no igaming_restore_loader role (nothing to drop)"
  fi

  if [ "$(rn_iam_only)" = 1 ]; then
    # SRP3R-2: on RDS igaming_backup is IAM-only (PASSWORD NULL, VALID UNTIL infinity, member of rds_iam). A finite validity or a missing rds_iam
    # membership means a password credential was put on it (e.g. reset-backup-credential.sql run on the wrong instance). Never make that password
    # permanent: remove it and restore the IAM-only state.
    left="$(rn_psql master "$DB_NAME" -c "SELECT COALESCE(rolvaliduntil::text, '') || '|' || (SELECT count(*) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid AND g.rolname = 'rds_iam') FROM pg_roles r WHERE rolname = 'igaming_backup'")"
    if [ -z "$left" ]; then
      rn_result cleanup_backup_credential INFO "igaming_backup does not exist (nothing to restore)"
    elif { [ "${left%%|*}" = "" ] || [ "${left%%|*}" = infinity ]; } && [ "${left##*|}" = 1 ]; then
      rn_result cleanup_backup_credential PASS "igaming_backup is IAM-only (no finite validity, member of rds_iam)"
    elif rn_psql master "$DB_NAME" -c "ALTER ROLE igaming_backup PASSWORD NULL VALID UNTIL 'infinity'" -c "GRANT rds_iam TO igaming_backup" >/dev/null 2>"${RN_SCRATCH}/cleanup.err"; then
      rn_result cleanup_backup_credential PASS "igaming_backup carried a password-style state (valid until '${left%%|*}', rds_iam members ${left##*|}); restored to IAM-only: PASSWORD NULL, VALID UNTIL infinity, member of rds_iam"
    else
      rn_result cleanup_backup_credential FAIL "could not restore igaming_backup to IAM-only: $(rn_first_line "${RN_SCRATCH}/cleanup.err"). The owner must remove the password and re-grant rds_iam by hand"
    fi
  else
    rn_result cleanup_backup_credential INFO "local harness: the backup login uses a password credential by design; not touched"
  fi
}
