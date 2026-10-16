#!/usr/bin/env bash
# Mode verify-db (STAGING-PREP-3): the READ-ONLY database verification pack. SOURCED by entrypoint.sh.
#
# Runs deploy/staging-runner/sql/verify-db.sql as the owner/master role and sql/verify-db-runtime.sql as a real
# igaming_runtime login, then `migrate verify`. Writes nothing. Parameters (all optional, non-secret):
#   EXPECT_BACKUP_ROLE   present | absent | any (default any): whether igaming_backup must exist yet
#   GRANT_LISTING        0 | 1 (default 0): also print the full sorted grant listing (GRANT| lines) for a manual diff
#   REQUIRE_ANCHOR       0 | 1 (default 0): 1 = a blank database_environment anchor is a FAIL (the seeding commands refuse it; plan 6.2/6.5)
# Always checked (SRP3-9): no igaming_restore_loader role, no restore-check (or other unexpected) database and, on RDS, no finite validity on
# igaming_backup remain: the proof that a drill cleaned up after itself (fix with the cleanup mode).
# Proves plan 3.1 (roles, grant snapshot), 3.2 (search_path, SECURITY DEFINER pins), 3.3 (TEMPORARY), 6.1 (schema_migrations
# not writable by the runtime role, migrate verify), 6.2 (environment anchor), FORCE RLS on every tenant table, the runtime role
# neither owner, superuser nor BYPASSRLS, and the migration head equal to the highest migration baked into this image.

# shellcheck shell=bash

mode_verify_db() {
  rn_need_tools psql sha256sum awk
  local expect_backup listing file_head file_count out rc
  expect_backup="$(rn_param EXPECT_BACKUP_ROLE '^(present|absent|any)$' any)"
  listing="$(rn_param GRANT_LISTING '^[01]$' 0)"
  local require_anchor
  require_anchor="$(rn_param REQUIRE_ANCHOR '^[01]$' 0)"
  rn_load_credentials
  rn_fetch_ca

  file_head="$(find "${RN_ROOT}/migrations" -maxdepth 1 -name '[0-9]*_*.up.sql' -printf '%f\n' | sed 's/_.*//' | sort -n | tail -1 | sed 's/^0*//')"
  file_count="$(find "${RN_ROOT}/migrations" -maxdepth 1 -name '[0-9]*_*.up.sql' | wc -l)"
  [[ "$file_head" =~ ^[0-9]+$ ]] || rn_die "the image holds no migrations"

  out="${RN_SCRATCH}/verify-db.out"
  if ! rn_psql owner "$DB_NAME" -v "owner_role=${DB_OWNER_USER}" -v "expect_backup=${expect_backup}" -v "active_kid=${ACTOR_PROOF_ACTIVE_KID:-unset}" \
        -v "file_head=${file_head}" -v "file_count=${file_count}" -v "listing=${listing}" -v "require_anchor=${require_anchor}" -v "iam_only=$(rn_iam_only)" -f "${RN_DIR}/sql/verify-db.sql" >"$out" 2>"${RN_SCRATCH}/verify-db.err"; then
    rn_emit_sql_output "$out"
    rn_stop "the verification pack could not run as ${DB_OWNER_USER}: $(rn_first_line "${RN_SCRATCH}/verify-db.err")"
  fi
  rn_emit_sql_output "$out"

  # the application's own role, really logged in
  if rn_psql runtime "$DB_NAME" -v "require_tls=$((1 - RN_LOCAL))" -f "${RN_DIR}/sql/verify-db-runtime.sql" >"${RN_SCRATCH}/verify-db-runtime.out" 2>"${RN_SCRATCH}/verify-db-runtime.err"; then
    rn_emit_sql_output "${RN_SCRATCH}/verify-db-runtime.out"
  else
    rn_result runtime_login FAIL "cannot log in as ${DB_RUNTIME_USER} with the injected runtime credential: $(rn_first_line "${RN_SCRATCH}/verify-db-runtime.err")"
  fi

  # migration checksums (cmd/migrate verify is read-only)
  if [ -x "${RN_BIN}/migrate" ]; then
    set +e
    PGPASSFILE="$RN_PGPASS" DATABASE_URL="$(rn_migrate_url owner)" "${RN_BIN}/migrate" -dir "${RN_ROOT}/migrations" verify >"${RN_SCRATCH}/migrate-verify.out" 2>&1
    rc=$?
    set -e
    if [ "$rc" -eq 0 ]; then rn_result migrate_verify PASS "$(tail -1 "${RN_SCRATCH}/migrate-verify.out")"
    else rn_result migrate_verify FAIL "exit ${rc}: $(tail -2 "${RN_SCRATCH}/migrate-verify.out" | tr '\n' ' ')"; fi
  else
    rn_result migrate_verify FAIL "the migrate binary is not in the image"
  fi
}
