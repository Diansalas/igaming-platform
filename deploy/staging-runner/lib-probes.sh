#!/usr/bin/env bash
# The denied-probe pack of the backup role (plan 5.3, ADR 0122 sections 3 and 5b): 15 write / escalation probes that MUST be denied,
# then 9 abuse-primitive probes that harden-backup-surface.sql denies, then 3 actor-proof KEY probes (F1, migration 0166, owner decision 5:
# the backup credential must not be able to read, lock or plant anything in the owner-only schema actorproof_private; judged like the write
# probes: only a privilege-denied SQLSTATE passes). SOURCED by modes/backup-restore-drill.sh, modes/pitr-verify.sh and
# modes/probe-backup-role.sh. Every probe runs as igaming_backup (IAM database authentication on RDS) in its OWN connection:
#   SET statement_timeout / SET lock_timeout (bounded: a probe that unexpectedly blocks cannot hang the task), then
#   BEGIN READ WRITE; <statement>; ROLLBACK
# (READ WRITE so the role's read-only session default cannot be what denies it; ROLLBACK so a probe that unexpectedly SUCCEEDS cannot
# change anything that is transactional).
#
# FAIL CLOSED (SRP3-2). The verdict comes from the SQLSTATE (psql -v VERBOSITY=sqlstate), never from message text:
#   PASS     the ONLY privilege-denied SQLSTATEs: 42501 insufficient_privilege, 0L000 invalid_grantor, 0LP01 invalid_grant_operation. The two
#            connect probes (a CONNECT denial happens before psql can report a SQLSTATE) accept exactly the server's CONNECT-privilege
#            message ("permission denied for database", "User does not have CONNECT privilege");
#   FAIL     the statement SUCCEEDED, or (write/escalation probes) it failed with any OTHER SQLSTATE: such an error is raised only AFTER the
#            privilege check passed (constraint, missing object, timeout, ...), so the role holds the privilege;
#   FINDING  an abuse-primitive probe that failed with another SQLSTATE (the probe may need updating), or succeeded because the environment
#            recorded that harden-backup-surface.sql was refused (EXPECT_HARDENING=refused: a residual per ADR 0122 section 5b);
#   BLOCKED  the probe could not run (connection or credential failure): it is not a pass.
# Completeness: rn_probe_pack counts the probes that actually ran (PASS, FAIL or FINDING). If fewer than all ran, probe_pack_complete is a
# FAIL, so a dead connection can never end in a zero-PASS exit 0.

# shellcheck shell=bash

RN_PROBE_RAN=0
RN_PROBE_EXPECTED=0
RN_PROBE_TIMEOUT_STMT="15s"
RN_PROBE_TIMEOUT_LOCK="5s"

# name|database|statement
RN_WRITE_PROBES=(
  "insert_tenants|*|INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES (gen_random_uuid(), 'x', 'x', 'under_platform_licence', 'active')"
  "update_ledger_entries|*|UPDATE ledger_entries SET amount = amount + 1"
  "delete_ledger_entries|*|DELETE FROM ledger_entries"
  "truncate_ledger_entries|*|TRUNCATE ledger_entries"
  "create_table|*|CREATE TABLE zz_backup_probe (a int)"
  "create_temp_table|*|CREATE TEMP TABLE zz_probe (a int)"
  "drop_table|*|DROP TABLE ledger_entries"
  "disable_trigger_all|*|ALTER TABLE ledger_entries DISABLE TRIGGER ALL"
  "set_role_owner|*|SET ROLE igaming"
  "set_session_replication_role|*|SET session_replication_role = replica"
  "create_role|*|CREATE ROLE zz_probe"
  "alter_role_superuser|*|ALTER ROLE igaming_backup SUPERUSER"
  "copy_to_program|*|COPY (SELECT 1) TO PROGRAM 'true'"
  "pg_read_file|*|SELECT pg_read_file('/etc/passwd')"
  "pg_reload_conf|*|SELECT pg_reload_conf()"
)
# F1 (migration 0166): the actor-proof signing key is out of the backup role's reach (no USAGE on the schema: 42501 before any table is named)
RN_KEY_PROBES=(
  "read_actor_proof_secret|*|SELECT secret FROM actorproof_private.actor_proof_keys"
  "lock_actor_proof_keys|*|LOCK TABLE actorproof_private.actor_proof_keys IN ACCESS SHARE MODE"
  "create_in_actorproof_private|*|CREATE TABLE actorproof_private.zz_backup_probe (a int)"
)
RN_ABUSE_PROBES=(
  "pg_advisory_lock|*|SELECT pg_advisory_lock(42)"
  "pg_advisory_lock_two_key|*|SELECT pg_advisory_lock(42, 43)"
  "pg_try_advisory_lock|*|SELECT pg_try_advisory_lock(42)"
  "pg_advisory_xact_lock|*|SELECT pg_advisory_xact_lock(42)"
  "pg_logical_emit_message|*|SELECT pg_logical_emit_message(false, 'x', 'y')"
  "lo_create|*|SELECT lo_create(0)"
  "pg_notify|*|SELECT pg_notify('c', 'p')"
  "connect_postgres|postgres|SELECT 1"
  "connect_template1|template1|SELECT 1"
)

# rn_probe_sqlstate <errfile> : the SQLSTATE psql reported ("ERROR:  42501" under VERBOSITY=sqlstate), or empty
rn_probe_sqlstate() { sed -n 's/^\(ERROR\|FATAL\):  \([0-9A-Z]\{5\}\)$/\2/p' "$1" 2>/dev/null | head -1; }

# rn_probe_privilege_state <sqlstate> : 0 when it is one of the privilege-denied states
rn_probe_privilege_state() { case "$1" in 42501|0L000|0LP01) return 0 ;; *) return 1 ;; esac; }

# rn_probe_verdict <class> <hard> <rc> <errfile> <name> <stmt>  ->  prints "STATUS|detail" (pure classification; unit-testable)
rn_probe_verdict() {
  local class="$1" hard="$2" rc="$3" err="$4" name="$5" stmt="$6" st first
  first="$(rn_first_line "$err")"
  st="$(rn_probe_sqlstate "$err")"
  case "$rc" in
    0)
      if [ "$class" = abuse ] && [ "$hard" = refused ]; then
        printf 'FINDING|NOT denied: RESIDUAL (ADR 0122 section 5b): harden-backup-surface.sql was refused by RDS, so the backup credential can still do this'
      else
        printf 'FAIL|NOT denied: the backup role could execute: %s' "$stmt"
      fi
      return 0 ;;
    1|3)
      # psql -c exits 1 when the statement fails (3 for scripts); 1 is also psql's own fatal error. The SQLSTATE line tells them apart: no
      # SQLSTATE at exit 1 means the statement never reached the server's error path.
      if [ "$rc" = 1 ] && [ -z "$st" ]; then printf 'BLOCKED|the probe did not run (client exit 1, no SQLSTATE): %s' "$first"; return 0; fi
      if rn_probe_privilege_state "$st"; then printf 'PASS|denied (SQLSTATE %s)' "$st"; return 0; fi
      if [ "$class" = write ] || [ "$class" = key ]; then
        printf 'FAIL|failed with SQLSTATE %s, which is not a privilege error: the error is raised only after the privilege check passed, so the role holds the privilege (or the probe is stale): %s' "${st:-unknown}" "$first"
      elif [ "$hard" = applied ] && [ "$st" != 42883 ] && [ "$st" != 0A000 ]; then
        # hardening is expected: an abuse primitive that got past the EXECUTE check (timeout 57014 / 55P03, anything else) is NOT denied (SRP3R-3)
        printf 'FAIL|failed with SQLSTATE %s after the privilege check passed, so the primitive is not denied although hardening is expected: %s' "${st:-unknown}" "$first"
      else
        printf 'FINDING|failed with SQLSTATE %s, which is not a privilege error (the probe may need updating, or hardening was refused): %s' "${st:-unknown}" "$first"
      fi
      return 0 ;;
    2)
      if [ "${name#connect_}" != "$name" ] && grep -qiE 'permission denied for database|User does not have CONNECT privilege' "$err"; then
        printf 'PASS|CONNECT denied: %s' "$first"; return 0
      fi
      printf 'BLOCKED|could not connect as the backup role, so the probe did not run: %s' "$first"; return 0 ;;
    *)
      printf 'BLOCKED|the probe did not run (client exit %s): %s' "$rc" "$first"; return 0 ;;
  esac
}

# rn_run_probe <name> <database> <statement> <class> <expect_hardening>   (class: write | abuse | key)
rn_run_probe() {
  local name="$1" db="$2" stmt="$3" class="$4" hard="$5" err rc=0 v
  [ "$db" != '*' ] || db="$DB_NAME"
  err="${RN_SCRATCH}/probe.${name}.err"
  rn_psql backup "$db" -v VERBOSITY=sqlstate \
    -c "SET statement_timeout = '${RN_PROBE_TIMEOUT_STMT}'" -c "SET lock_timeout = '${RN_PROBE_TIMEOUT_LOCK}'" \
    -c "BEGIN READ WRITE; ${stmt}; ROLLBACK" >/dev/null 2>"$err" || rc=$?
  v="$(rn_probe_verdict "$class" "$hard" "$rc" "$err" "$name" "$stmt")"
  case "${v%%|*}" in PASS|FAIL|FINDING) RN_PROBE_RAN=$((RN_PROBE_RAN + 1)) ;; esac
  rn_result "probe_${class}_${name}" "${v%%|*}" "${v#*|}"
}

# rn_probe_pack <expect_hardening: applied|refused|unknown>
rn_probe_pack() {
  local hard="${1:-applied}" p name db stmt
  RN_PROBE_RAN=0
  RN_PROBE_EXPECTED=$(( ${#RN_WRITE_PROBES[@]} + ${#RN_ABUSE_PROBES[@]} + ${#RN_KEY_PROBES[@]} ))
  for p in "${RN_WRITE_PROBES[@]}"; do IFS='|' read -r name db stmt <<<"$p"; rn_run_probe "$name" "$db" "$stmt" write "$hard"; done
  for p in "${RN_ABUSE_PROBES[@]}"; do IFS='|' read -r name db stmt <<<"$p"; rn_run_probe "$name" "$db" "$stmt" abuse "$hard"; done
  for p in "${RN_KEY_PROBES[@]}"; do IFS='|' read -r name db stmt <<<"$p"; rn_run_probe "$name" "$db" "$stmt" key "$hard"; done
  if [ "$RN_PROBE_RAN" -eq "$RN_PROBE_EXPECTED" ]; then
    rn_result probe_pack_complete PASS "all ${RN_PROBE_EXPECTED} probes ran"
  else
    rn_result probe_pack_complete FAIL "only ${RN_PROBE_RAN} of ${RN_PROBE_EXPECTED} probes ran (the rest were BLOCKED: no connection or credential): the denial of the backup role is NOT demonstrated"
  fi
}

# rn_backup_role_attributes : the role as the role sees itself (plan 5.2): NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 2,
# memberships only rds_iam (IAM authentication on RDS) or none
rn_backup_role_attributes() {
  local a
  if ! a="$(rn_psql backup "$DB_NAME" -F, -c "SET statement_timeout = '${RN_PROBE_TIMEOUT_STMT}'" -c "SET lock_timeout = '${RN_PROBE_TIMEOUT_LOCK}'" -c "SELECT rolsuper::int, rolbypassrls::int, rolcreatedb::int, rolcreaterole::int, rolreplication::int, rolconnlimit, COALESCE((SELECT string_agg(g.rolname, '+' ORDER BY g.rolname) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid), 'none'), COALESCE(rolvaliduntil::text, 'none') FROM pg_roles r WHERE rolname = 'igaming_backup'" 2>"${RN_SCRATCH}/attrs.err")"; then
    rn_result backup_role_attributes BLOCKED "cannot read the role's own attributes as igaming_backup: $(rn_first_line "${RN_SCRATCH}/attrs.err")"
    return 0
  fi
  a="$(printf '%s\n' "$a" | grep -v '^SET$' | head -1)"
  case "$a" in
    0,1,0,0,0,2,none,*|0,1,0,0,0,2,rds_iam,*) rn_result backup_role_attributes PASS "NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 2; membership: $(printf '%s' "$a" | cut -d, -f7); valid until: $(printf '%s' "$a" | cut -d, -f8)" ;;
    *) rn_result backup_role_attributes FAIL "unexpected attributes (super,bypassrls,createdb,createrole,replication,connlimit,member-of,valid-until): ${a}" ;;
  esac
}
