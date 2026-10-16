#!/usr/bin/env bash
# Mode record-anchor (STAGING-PREP-3, SRP3-1; plan 6.2): record the write-once database_environment anchor (migration 0135) so that the seeding
# commands (seed-market, seed-world), which refuse any database whose anchor is not staging / development / test (no row = production), can
# run. This is the sanctioned path: the runner's owner connection is the table owner, the only role the database lets write the row.
# SOURCED.
#
# Parameters (non-secret; all required unless marked):
#   ANCHOR_VALUE  staging (default) | development | test. NEVER production (recording production belongs to the production provisioning runbook)
#   AUTH_REF      the authorisation reference (ticket id)
#   OPERATOR      who runs it;  REASON  why  (both are recorded in the run log with the run's task id)
# Idempotent: the same value already recorded is a no-op; a different value is a FAIL (the row can never change). Same gating as seed-world:
# APP_ENV development or staging (the container's own setting) and the owner credential only. Restore consequence (backup-restore-dr.md): a
# restored copy's anchor is blank by design, and restore.sh requires target environment = source environment.

# shellcheck shell=bash

mode_record_anchor() {
  rn_need_tools psql
  local value authref operator reason out="${RN_SCRATCH}/anchor.out" state=""
  value="$(rn_param ANCHOR_VALUE '^(staging|development|test)$' staging)"
  authref="$(rn_param AUTH_REF "$RN_RE_AUTHREF")"
  operator="$(rn_param OPERATOR "$RN_RE_OPERATOR")"
  reason="$(rn_param REASON "$RN_RE_REASON")"
  case "${APP_ENV}:${value}" in
    staging:development|staging:test) rn_die "ANCHOR_VALUE=${value} is not allowed with APP_ENV=staging: a staging database is anchored staging" ;;
  esac
  rn_load_credentials
  rn_fetch_ca
  rn_result record_anchor_request INFO "anchor=${value} authorisation=${authref} operator=${operator} reason=${reason}"
  if ! rn_psql owner "$DB_NAME" -v "anchor=${value}" -f "${RN_DIR}/sql/record-anchor.sql" >"$out" 2>"${RN_SCRATCH}/anchor.err"; then
    rn_result record_anchor FAIL "could not record the anchor as ${DB_OWNER_USER}: $(rn_first_line "${RN_SCRATCH}/anchor.err")"
    return 0
  fi
  state="$(sed -n 's/^ANCHOR|state|//p' "$out" | head -1)"
  case "$state" in
    inserted) rn_result record_anchor PASS "database_environment recorded as ${value} (write-once; recorded_by is the owner login)" ;;
    already_recorded_same) rn_result record_anchor PASS "database_environment is already ${value}; nothing written (idempotent)" ;;
    conflict_other_value:*) rn_result record_anchor FAIL "database_environment is already recorded as '${state#conflict_other_value:}'; it can never be changed: nothing written" ;;
    no_table) rn_result record_anchor FAIL "table database_environment does not exist: migrations have not run (migration 0135)" ;;
    *) rn_result record_anchor FAIL "unexpected state '${state}'" ;;
  esac
  grep '^ANCHOR|row|' "$out" | while IFS= read -r l; do printf '%s\n' "$(rn_wire "$l")"; done
}
