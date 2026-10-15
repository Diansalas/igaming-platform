#!/usr/bin/env bash
# Shared helpers for deploy/backup/*.sh (A12, ADR 0122). SOURCED, not executed.
#
# Target selection is by BACKUP_* variables only (never ambient PG* variables, which are unset so a stray
# PGHOST in the operator's shell cannot redirect a backup or a restore):
#   BACKUP_PGHOST      host (127.0.0.1 for the throwaway drill clusters)
#   BACKUP_PGPORT      port
#   BACKUP_PGDATABASE  database name
#   BACKUP_PGUSER      role to connect as (igaming_backup for backup.sh; the restore role for restore.sh)
#   BACKUP_PGPASSFILE  path to a libpq passfile (0600). The password is NEVER passed in argv or in a URL;
#                      the operator/CI job materialises this file from the secret store (see ADR 0122).
#   BACKUP_PGSSLMODE   libpq sslmode (default: prefer for loopback, require otherwise)
#
# Target guard (fail closed). The shared local development/CI cluster listens on 127.0.0.1:5432 and is OFF
# LIMITS to every script in this directory: a loopback target must be one of the throwaway drill ports
# (BACKUP_DRILL_PORTS, default "5499 5498 5497"). A non-loopback target (the staging RDS instance, later) is
# refused unless BACKUP_ALLOW_REMOTE_TARGET is set to EXACTLY the host name being used - an explicit,
# per-invocation acknowledgement, documented in docs/runbooks/backup-restore-dr.md.

# shellcheck shell=bash

bk_die() { echo "backup: $*" >&2; exit 2; }
bk_log() { echo "backup: $*"; }

# libpq must see only what we set below.
unset PGHOST PGHOSTADDR PGPORT PGSERVICE PGSERVICEFILE PGSYSCONFDIR PGOPTIONS PGTARGETSESSIONATTRS \
  PGDATABASE PGUSER PGPASSWORD PGPASSFILE PGSSLMODE PGSSLROOTCERT PGCONNECT_TIMEOUT

bk_is_loopback() { case "$1" in 127.0.0.1|localhost|::1) return 0 ;; *) return 1 ;; esac; }

# bk_guard_target host port : refuse the shared cluster and anything not explicitly allowed.
bk_guard_target() {
  local host="$1" port="$2" p ok=0
  [ -n "$host" ] || bk_die "BACKUP_PGHOST is required"
  case "$port" in ''|*[!0-9]*) bk_die "BACKUP_PGPORT must be an integer" ;; esac
  case "$host" in /*) bk_die "unix-socket hosts are not accepted for the operator scripts; use 127.0.0.1 and a drill port" ;; esac
  if bk_is_loopback "$host"; then
    [ "$port" != 5432 ] || bk_die "refusing 127.0.0.1:5432: that is the SHARED local cluster used by all tests (CLAUDE.md environment safety). Use a throwaway drill cluster (deploy/backup/drill.sh)"
    for p in ${BACKUP_DRILL_PORTS:-5499 5498 5497}; do [ "$p" = "$port" ] && ok=1; done
    [ "$ok" -eq 1 ] || bk_die "loopback port ${port} is not a throwaway drill port (${BACKUP_DRILL_PORTS:-5499 5498 5497}); refusing"
  else
    [ "${BACKUP_ALLOW_REMOTE_TARGET:-}" = "$host" ] \
      || bk_die "remote host '${host}' refused: set BACKUP_ALLOW_REMOTE_TARGET=${host} to acknowledge this is the intended (staging) target. Production is not authorised."
  fi
}

# bk_connect_env : validate BACKUP_* and export the libpq environment for child processes.
bk_connect_env() {
  : "${BACKUP_PGHOST:?BACKUP_PGHOST is required}"
  : "${BACKUP_PGPORT:?BACKUP_PGPORT is required}"
  : "${BACKUP_PGDATABASE:?BACKUP_PGDATABASE is required}"
  : "${BACKUP_PGUSER:?BACKUP_PGUSER is required}"
  bk_guard_target "$BACKUP_PGHOST" "$BACKUP_PGPORT"
  local pf="${BACKUP_PGPASSFILE:-}"
  [ -n "$pf" ] || bk_die "BACKUP_PGPASSFILE is required (path to a 0600 libpq passfile materialised from the secret store; no password is accepted in the environment, argv or a URL)"
  [ -f "$pf" ] || bk_die "credential file missing: ${pf} (the backup credential was not provisioned, or it expired and was removed)"
  local mode
  mode="$(stat -c %a "$pf")"
  case "$mode" in 600|400) ;; *) bk_die "credential file ${pf} must be mode 0600 or 0400 (is ${mode})" ;; esac
  export PGHOST="$BACKUP_PGHOST" PGPORT="$BACKUP_PGPORT" PGDATABASE="$BACKUP_PGDATABASE" PGUSER="$BACKUP_PGUSER" PGPASSFILE="$pf"
  export PGCONNECT_TIMEOUT=10
  if bk_is_loopback "$BACKUP_PGHOST"; then export PGSSLMODE="${BACKUP_PGSSLMODE:-prefer}"; else export PGSSLMODE="${BACKUP_PGSSLMODE:-require}"; fi
}

bk_need_tools() { local t; for t in "$@"; do command -v "$t" >/dev/null 2>&1 || bk_die "required tool not found: $t"; done; }

# bk_sql <sql> : run one statement against the connected target, tuples only.
bk_sql() { psql -X -q -At -v ON_ERROR_STOP=1 -c "$1"; }

now_s() { date +%s.%N; }
elapsed_s() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }
