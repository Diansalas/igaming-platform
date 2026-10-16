#!/usr/bin/env bash
# The denied-probe pack of the backup role (plan 5.3, ADR 0122 sections 3 and 5b): 15 write / escalation probes that MUST be denied,
# then 8 abuse-primitive probes that harden-backup-surface.sql denies. SOURCED by modes/backup-restore-drill.sh and
# modes/probe-backup-role.sh. Every probe runs as igaming_backup (IAM database authentication on RDS) in its OWN connection inside
#   BEGIN READ WRITE; <statement>; ROLLBACK
# (READ WRITE so the role's read-only session default cannot be what denies it; ROLLBACK so a probe that unexpectedly SUCCEEDS cannot
# change anything that is transactional). One RESULT per probe:
#   PASS     denied, and by a privilege error (permission denied / must be owner / must be superuser / only roles with ...);
#   FAIL     the statement SUCCEEDED (a privilege the backup role must not have);
#   FINDING  the statement failed for another reason (not a privilege error: the probe itself may need updating), or, for the abuse
#            primitives only, succeeded because the environment recorded that harden-backup-surface.sql was refused (EXPECT_HARDENING=refused:
#            a residual per ADR 0122 section 5b, not a script failure).

# shellcheck shell=bash

RN_PRIV_RE='permission denied|must be owner|must be superuser|only roles with|insufficient[ _]privilege|must have the [A-Z]+ attribute|permission to'

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
RN_ABUSE_PROBES=(
  "pg_advisory_lock|*|SELECT pg_advisory_lock(42)"
  "pg_try_advisory_lock|*|SELECT pg_try_advisory_lock(42)"
  "pg_advisory_xact_lock|*|SELECT pg_advisory_xact_lock(42)"
  "pg_logical_emit_message|*|SELECT pg_logical_emit_message(false, 'x', 'y')"
  "lo_create|*|SELECT lo_create(0)"
  "pg_notify|*|SELECT pg_notify('c', 'p')"
  "connect_postgres|postgres|SELECT 1"
  "connect_template1|template1|SELECT 1"
)

# rn_run_probe <name> <database> <statement> <class> <expect_hardening>   (class: write | abuse)
rn_run_probe() {
  local name="$1" db="$2" stmt="$3" class="$4" hard="$5" err first
  [ "$db" != '*' ] || db="$DB_NAME"
  err="${RN_SCRATCH}/probe.${name}.err"
  if rn_psql backup "$db" -c "BEGIN READ WRITE; ${stmt}; ROLLBACK" >/dev/null 2>"$err"; then
    if [ "$class" = abuse ] && [ "$hard" = refused ]; then
      rn_result "probe_${class}_${name}" FINDING "NOT denied: RESIDUAL (ADR 0122 section 5b): harden-backup-surface.sql was refused by RDS, so the backup credential can still do this"
    else
      rn_result "probe_${class}_${name}" FAIL "NOT denied: the backup role could execute: ${stmt}"
    fi
    return 0
  fi
  first="$(rn_first_line "$err")"
  if grep -qiE "$RN_PRIV_RE" "$err"; then
    rn_result "probe_${class}_${name}" PASS "denied: ${first}"
  elif grep -qiE 'password authentication failed|no pg_hba|PAM authentication failed|could not connect|connection refused|timeout expired|SSL' "$err" && [ "$class" != abuse ]; then
    rn_result "probe_${class}_${name}" BLOCKED "could not connect as the backup role, so the probe did not run: ${first}"
  else
    rn_result "probe_${class}_${name}" FINDING "failed, but not by a privilege error (the probe may need updating): ${first}"
  fi
}

# rn_probe_pack <expect_hardening: applied|refused|unknown>
rn_probe_pack() {
  local hard="${1:-applied}" p name db stmt
  for p in "${RN_WRITE_PROBES[@]}"; do IFS='|' read -r name db stmt <<<"$p"; rn_run_probe "$name" "$db" "$stmt" write "$hard"; done
  for p in "${RN_ABUSE_PROBES[@]}"; do IFS='|' read -r name db stmt <<<"$p"; rn_run_probe "$name" "$db" "$stmt" abuse "$hard"; done
}

# rn_backup_role_attributes : the role as the role sees itself (plan 5.2): NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 2,
# memberships only rds_iam (IAM authentication on RDS) or none
rn_backup_role_attributes() {
  local a
  if ! a="$(rn_psql backup "$DB_NAME" -F, -c "SELECT rolsuper::int, rolbypassrls::int, rolcreatedb::int, rolcreaterole::int, rolreplication::int, rolconnlimit, COALESCE((SELECT string_agg(g.rolname, '+' ORDER BY g.rolname) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid), 'none'), COALESCE(rolvaliduntil::text, 'none') FROM pg_roles r WHERE rolname = 'igaming_backup'" 2>"${RN_SCRATCH}/attrs.err")"; then
    rn_result backup_role_attributes BLOCKED "cannot read the role's own attributes as igaming_backup: $(rn_first_line "${RN_SCRATCH}/attrs.err")"
    return 0
  fi
  case "$a" in
    0,1,0,0,0,2,none,*|0,1,0,0,0,2,rds_iam,*) rn_result backup_role_attributes PASS "NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 2; membership: $(printf '%s' "$a" | cut -d, -f7); valid until: $(printf '%s' "$a" | cut -d, -f8)" ;;
    *) rn_result backup_role_attributes FAIL "unexpected attributes (super,bypassrls,createdb,createrole,replication,connlimit,member-of,valid-until): ${a}" ;;
  esac
}
