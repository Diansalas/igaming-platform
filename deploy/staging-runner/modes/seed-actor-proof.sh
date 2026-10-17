#!/usr/bin/env bash
# Mode seed-actor-proof (STAGING-PREP-3, owner action B of aws-staging-deploy-plan.md): write the actor_proof_keys verification row
# from the Secrets Manager secret igaming-staging/actor-proof-keys, following operational-runbooks.md section 15 step 4. SOURCED.
#
# Runs ONLY in the staging-runner-actor-proof task definition: it alone receives ACTOR_PROOF_KEYS (the secret, "kid:base64", injected
# by the execution role) and has no task role. Refuses unless APP_ENV is development or staging. No parameters.
#
# What it does, in order:
#   1. parse the secret: the entry whose kid equals ACTOR_PROOF_ACTIVE_KID (the kid inside the secret must be that kid); decode the
#      base64 to bytes (>= 32 required by the table's CHECK); hex-encode into a 0600 file on tmpfs; unset the variable;
#   2. refuse if the server would log statement parameters (the runbook's pre-check: log_statement mod/all, log_min_duration_statement,
#      log_duration, log_parameter_max_length_on_error, pgaudit parameter logging);
#   3. as the owner/master, run sql/seed-actor-proof.sql: parameter-bound insert (\bind), idempotent, never overwrites;
#   4. report kid / status / octet length only, then remove the files.
# The key material is never on a command line, in the process list, in a URL or in a log: it lives in the environment of PID 1 (as
# every ECS secret does), one tmpfs file, and the client's memory.

# shellcheck shell=bash

mode_seed_actor_proof() {
  rn_need_tools psql base64 od tr wc
  local kid entries e k b64="" n=0 hexfile binfile size oldifs psql_major
  kid="$(rn_param ACTOR_PROOF_ACTIVE_KID '^[A-Za-z0-9._-]{1,32}$')"
  [ -n "${RN_S_ACTOR_KEYS:-}" ] || rn_die "ACTOR_PROOF_KEYS is not injected: this mode runs only in the staging-runner-actor-proof task definition"
  psql_major="$(psql --version | sed -n 's/.* \([0-9][0-9]*\)\..*/\1/p')"
  [ "${psql_major:-0}" -ge 16 ] || rn_die "psql 16 or later is required for the parameter-bound insert (\\bind)"

  # 1. the entry for the active kid
  oldifs="$IFS"; IFS=','; read -ra entries <<<"$RN_S_ACTOR_KEYS"; IFS="$oldifs"
  for e in "${entries[@]}"; do
    k="${e%%:*}"
    if [ "$k" = "$kid" ]; then b64="${e#*:}"; n=$((n + 1)); fi
  done
  RN_S_ACTOR_KEYS=""; unset entries e
  if [ "$n" -ne 1 ] || [ -z "$b64" ]; then
    rn_stop "the actor-proof secret holds ${n} entries for kid ${kid}: the kid inside igaming-staging/actor-proof-keys must equal ACTOR_PROOF_ACTIVE_KID (terraform output key_family_kid); nothing was written"
  fi
  rn_register_secret "$b64"
  binfile="${RN_SHM}/ap.bin"; hexfile="${RN_SHM}/ap.hex"
  if ! printf '%s' "$b64" | base64 -d >"$binfile" 2>/dev/null; then rn_stop "the secret's key part is not valid base64; nothing was written"; fi
  unset b64
  size="$(wc -c <"$binfile" | tr -d ' ')"
  [ "$size" -ge 32 ] || rn_stop "the decoded key is ${size} bytes; at least 32 are required (actor_proof_keys CHECK); nothing was written"
  od -An -v -tx1 "$binfile" | tr -d ' \n' >"$hexfile"
  rm -f "$binfile"
  chmod 600 "$hexfile"
  rn_register_secret "$(cat "$hexfile")"

  rn_load_credentials
  rn_fetch_ca

  # 2. the runbook's pre-check (extended, SRP3-5): nothing the server could log may carry the bound parameter. Reads use current_setting(..., true),
  # so a library that is not loaded (pgaudit, auto_explain) simply reads as unset.
  local logchk
  if ! logchk="$(rn_psql owner "$DB_NAME" -F '|' -c "SELECT current_setting('log_statement'), current_setting('log_min_duration_statement'), current_setting('log_duration'), COALESCE(current_setting('log_parameter_max_length_on_error', true), '0'), (current_setting('log_transaction_sample_rate')::float8 <> 0)::int, (current_setting('log_statement_sample_rate')::float8 <> 0 AND current_setting('log_min_duration_sample') <> '-1')::int, (COALESCE(current_setting('auto_explain.log_min_duration', true), '-1') <> '-1' AND COALESCE(current_setting('auto_explain.log_parameter_max_length', true), '-1') <> '0')::int, (COALESCE(NULLIF(current_setting('pgaudit.log', true), ''), 'none') <> 'none' AND COALESCE(NULLIF(current_setting('pgaudit.log_parameter', true), ''), 'off') IN ('on', 'true', '1'))::int, (NULLIF(current_setting('pgaudit.role', true), '') IS NOT NULL AND COALESCE(NULLIF(current_setting('pgaudit.log_parameter', true), ''), 'off') IN ('on', 'true', '1'))::int, COALESCE(NULLIF(current_setting('pgaudit.log', true), ''), 'none')" 2>"${RN_SCRATCH}/logchk.err")"; then
    rn_stop "cannot connect as ${DB_OWNER_USER} to check the statement-logging settings: $(rn_first_line "${RN_SCRATCH}/logchk.err")"
  fi
  local l_stmt l_dur l_logdur l_onerr l_txn l_sample l_autoexp l_pga l_pgaobj l_pgalog
  IFS='|' read -r l_stmt l_dur l_logdur l_onerr l_txn l_sample l_autoexp l_pga l_pgaobj l_pgalog <<<"$logchk"
  rn_result actor_proof_logging_precheck INFO "log_statement=${l_stmt} log_min_duration_statement=${l_dur} log_duration=${l_logdur} log_parameter_max_length_on_error=${l_onerr} txn_sample=${l_txn} stmt_sample=${l_sample} auto_explain_params=${l_autoexp} pgaudit_param_logging=${l_pga} pgaudit_object_param_logging=${l_pgaobj} pgaudit.log=${l_pgalog}"
  case "$l_stmt" in mod|all) rn_stop "log_statement=${l_stmt}: the secret would be written to the database log; nothing was written (operational-runbooks.md section 15 step 4)" ;; esac
  [ "$l_dur" = "-1" ] || rn_stop "log_min_duration_statement=${l_dur}: a slow statement could be logged with its parameters; nothing was written"
  [ "$l_logdur" = "off" ] || rn_stop "log_duration=on: nothing was written"
  [ "$l_onerr" = "0" ] || rn_stop "log_parameter_max_length_on_error=${l_onerr}: a failing statement would log its parameters; nothing was written"
  [ "$l_txn" = "0" ] || rn_stop "log_transaction_sample_rate is not 0: a sampled transaction logs every statement with its parameters; nothing was written"
  [ "$l_sample" = "0" ] || rn_stop "sampled statement logging (log_statement_sample_rate with log_min_duration_sample) is on; nothing was written"
  [ "$l_autoexp" = "0" ] || rn_stop "auto_explain logs parameters (auto_explain.log_min_duration / log_parameter_max_length); nothing was written"
  [ "$l_pga" = "0" ] || rn_stop "pgaudit logs parameters (pgaudit.log=${l_pgalog}, pgaudit.log_parameter=on); nothing was written"
  [ "$l_pgaobj" = "0" ] || rn_stop "pgaudit object auditing (pgaudit.role) logs parameters (pgaudit.log_parameter=on); nothing was written"
  rn_result actor_proof_logging_safe PASS "the database would not log the bound parameter"

  # 3. the insert
  local out="${RN_SCRATCH}/actor.out" state="" row_ok=0 line
  if ! RN_AP_HEX_FILE="$hexfile" rn_psql owner "$DB_NAME" -v "ap_kid=${kid}" -f "${RN_DIR}/sql/seed-actor-proof.sql" >"$out" 2>"${RN_SCRATCH}/actor.err"; then
    rm -f "$hexfile"
    rn_result actor_proof_row FAIL "the insert failed: $(rn_first_line "${RN_SCRATCH}/actor.err")"
    return 0
  fi
  rm -f "$hexfile"
  while IFS= read -r line; do
    case "$line" in
      ACTOR\|state\|*) state="${line#ACTOR|state|}" ;;
      ACTOR\|row\|"${kid}"\|active\|octets=*) row_ok=1; printf 'ACTOR|row|kid=%s status=active %s\n' "$kid" "${line##*|}" ;;
      ACTOR\|row\|*) printf 'ACTOR|row|%s\n' "$(rn_oneline "${line#ACTOR|row|}")" ;;
    esac
  done <"$out"
  case "$state" in
    inserted) rn_result actor_proof_row PASS "kid ${kid} inserted (parameter-bound, no key material in any statement text or log)" ;;
    already_present_identical) rn_result actor_proof_row PASS "kid ${kid} already present with the identical secret; nothing written (idempotent)" ;;
    conflict_different_secret_for_this_kid) rn_result actor_proof_row FAIL "kid ${kid} exists with a DIFFERENT secret; refusing to overwrite (rotation is a separate reviewed procedure, operational-runbooks.md section 15)" ;;
    no_table) rn_result actor_proof_row FAIL "table actorproof_private.actor_proof_keys does not exist: migrations have not run (migrations 0120 and 0166)" ;;
    *) rn_result actor_proof_row FAIL "unexpected state '${state}'" ;;
  esac
  if [ "$row_ok" = 1 ]; then rn_result actor_proof_row_verified PASS "kid ${kid}: status active, octet_length >= 32 (governed writes no longer answer AP002 once platform-api's ACTOR_PROOF_KEYS carries the same kid)"
  elif [ "$state" = inserted ] || [ "$state" = already_present_identical ]; then rn_result actor_proof_row_verified FAIL "the row is not active after the write"; fi
}
