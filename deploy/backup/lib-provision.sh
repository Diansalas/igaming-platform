#!/usr/bin/env bash
# Role + database provisioning for the THROWAWAY drill clusters. SOURCED after lib-common.sh and lib-cluster.sh.
# Applies deploy/init-app-role.sql (the repository's role model) unchanged, then replaces its documented dev
# passwords with random per-run secrets, then provisions the dedicated backup role (init-backup-role.sql).
# Everything here targets a cluster created by lib-cluster.sh; cl_super talks to that cluster's private socket.

# shellcheck shell=bash

BK_DIR="${BK_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
BK_REPO="${BK_REPO:-$(cd "${BK_DIR}/../.." && pwd)}"
SRC_DB_NAME="igaming_platform_dev"     # the name deploy/init-app-role.sql hard-codes

# prov_set_password name port role : generate a random password, set it, append a passfile line to
# ${DRILL_ROOT}/secrets/pgpass.<file> (file = $4, default pgpass). The password only ever travels through
# an environment variable into a psql variable.
prov_set_password() {
  local name="$1" port="$2" role="$3" file="${4:-pgpass}" pw
  pw="$(cl_random_secret)"
  IGPW="$pw" cl_super "$name" "$port" -d postgres <<SQL >/dev/null
\\getenv pw IGPW
ALTER ROLE ${role} WITH PASSWORD :'pw';
SQL
  cl_write_pass "$role" "$pw" "$port" "${DRILL_ROOT}/secrets/${file}"
}

# prov_roles name port : database + igaming + igaming_runtime (deploy/init-app-role.sql) with random passwords.
prov_roles() {
  local name="$1" port="$2"
  cl_super "$name" "$port" -d postgres -c "CREATE DATABASE ${SRC_DB_NAME}"
  cl_super "$name" "$port" -d "$SRC_DB_NAME" -f "${BK_REPO}/deploy/init-app-role.sql" >/dev/null
  prov_set_password "$name" "$port" igaming pgpass
  prov_set_password "$name" "$port" igaming_runtime pgpass
}

# prov_reapply_app_roles name port : the post-migration backfill the dev harness runs (`make dev-db-init-roles`).
# The first statement of init-app-role.sql (CREATE ROLE igaming) is not guarded, so exactly that one error is
# expected and tolerated; any other error text fails the step.
prov_reapply_app_roles() {
  local name="$1" port="$2" out
  out="$(cl_super "$name" "$port" -d "$SRC_DB_NAME" -v ON_ERROR_STOP=0 -f "${BK_REPO}/deploy/init-app-role.sql" 2>&1 >/dev/null || true)"
  if printf '%s\n' "$out" | grep -i 'error' | grep -vq 'role "igaming" already exists'; then
    printf '%s\n' "$out" >&2
    bk_die "unexpected error re-applying deploy/init-app-role.sql after migrations"
  fi
}

# prov_backup_role name port [valid_until] : create/refresh igaming_backup; its passfile is separate.
prov_backup_role() {
  local name="$1" port="$2" until="${3:-}" pw
  pw="$(cl_random_secret)"
  : >"${DRILL_ROOT}/secrets/pgpass.backup"
  IGAMING_BACKUP_PASSWORD="$pw" BACKUP_ROLE_VALID_UNTIL="$until" \
    cl_super "$name" "$port" -d "$SRC_DB_NAME" -f "${BK_DIR}/init-backup-role.sql" >/dev/null
  cl_write_pass igaming_backup "$pw" "$port" "${DRILL_ROOT}/secrets/pgpass.backup"
}

# prov_restore_role name port : the restore operator for the drill - CREATEDB, member of the owner role igaming,
# no superuser. (On RDS the restore role is the master user `igaming` itself.)
prov_restore_role() {
  local name="$1" port="$2"
  cl_super "$name" "$port" -d postgres -c "CREATE ROLE drill_restore LOGIN NOSUPERUSER CREATEDB NOCREATEROLE NOREPLICATION IN ROLE igaming" >/dev/null
  prov_set_password "$name" "$port" drill_restore pgpass.restore
}

# prov_replication_role name port : REPLICATION login used only by pg_basebackup in the PITR drill.
prov_replication_role() {
  local name="$1" port="$2"
  cl_super "$name" "$port" -d postgres -c "CREATE ROLE drill_replicator LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE REPLICATION" >/dev/null
  prov_set_password "$name" "$port" drill_replicator pgpass.repl
}
