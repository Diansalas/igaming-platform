#!/usr/bin/env bash
# Throwaway PostgreSQL 16 cluster management for the A12 backup/PITR drill. SOURCED by deploy/backup/drill.sh
# and deploy/backup/pitr-drill.sh. DRILL ONLY.
#
# SAFETY (CLAUDE.md "Environment safety", incident-2026-09-27):
#  * Every cluster lives in its OWN directory under a fresh /tmp/pgdrill-XXXXXX root created by this script and
#    is listed in DRILL_CLUSTERS; the shared cluster on 5432 is never contacted by anything here (the port
#    guard refuses it, see cl_check_port).
#  * initdb refuses to run as root, so the cluster processes run as the existing OS user `postgres` via
#    `runuser -u postgres` (a privilege DROP). No sudo, no pkill (clusters are stopped with pg_ctl on their
#    own data directory), no change to the shared cluster's roles or passwords.
#  * Roles created here exist only inside the throwaway cluster and die with it. Their passwords are random per
#    run, written only to 0600 passfiles inside the drill root, and never to the repository, argv or logs.
#  * cl_teardown (installed on EXIT/INT/TERM by the drill scripts) stops every registered cluster with
#    `pg_ctl stop -m fast` and removes the root. It refuses to remove anything that is not /tmp/pgdrill-*.

# shellcheck shell=bash

PG_BIN="${PG_BIN:-/usr/lib/postgresql/16/bin}"
DRILL_ROOT=""
DRILL_CLUSTERS=()   # names, in start order

pgas() { runuser -u postgres -- "$@"; }

cl_check_port() {
  case "$1" in ''|*[!0-9]*) bk_die "port must be an integer" ;; esac
  [ "$1" != 5432 ] || bk_die "refusing port 5432 (shared cluster) for a throwaway cluster"
}

cl_new_root() {
  [ -x "${PG_BIN}/initdb" ] || bk_die "PostgreSQL 16 server binaries not found at ${PG_BIN}"
  command -v runuser >/dev/null 2>&1 || bk_die "runuser is not available; the throwaway cluster cannot be started (initdb refuses root). Stop and report; no other escalation is attempted"
  id -u postgres >/dev/null 2>&1 || bk_die "OS user postgres does not exist"
  DRILL_ROOT="$(mktemp -d /tmp/pgdrill-XXXXXX)"
  chown postgres:postgres "$DRILL_ROOT"
  chmod 711 "$DRILL_ROOT"            # postgres owns it; others may traverse (client files below are 0600/0700 root-owned)
  mkdir -p "${DRILL_ROOT}/secrets" "${DRILL_ROOT}/work"
  chmod 700 "${DRILL_ROOT}/secrets" "${DRILL_ROOT}/work"
  bk_log "throwaway root ${DRILL_ROOT}"
}

# cl_init name port : create + configure (not start) cluster ${DRILL_ROOT}/name on 127.0.0.1:port.
# Optional third argument: extra postgresql.conf lines.
cl_init() {
  local name="$1" port="$2" extra="${3:-}" d
  cl_check_port "$port"
  d="${DRILL_ROOT}/${name}"
  mkdir -p "${d}/sock" "${d}/wal_archive"
  chown postgres:postgres "$d" "${d}/sock" "${d}/wal_archive"
  chmod 700 "${d}" "${d}/sock" "${d}/wal_archive"
  pgas "${PG_BIN}/initdb" -D "${d}/data" -U postgres --auth-local=trust --auth-host=scram-sha-256 -E UTF8 --locale=C.UTF-8 --data-checksums >"${d}/initdb.log" 2>&1 \
    || { tail -5 "${d}/initdb.log" >&2; bk_die "initdb failed for ${name}"; }
  cat >"${d}/data/pg_hba.conf" <<EOF
local   all         all                       trust
host    all         all   127.0.0.1/32        scram-sha-256
host    replication all   127.0.0.1/32        scram-sha-256
EOF
  cat >>"${d}/data/postgresql.conf" <<EOF

# ---- A12 throwaway drill cluster ${name} ----
port = ${port}
listen_addresses = '127.0.0.1'
unix_socket_directories = '${d}/sock'
max_connections = 60
shared_buffers = 256MB
wal_level = replica
wal_compression = on
max_wal_senders = 4
min_wal_size = 80MB
max_wal_size = 512MB
checkpoint_timeout = 5min
log_connections = on
log_disconnections = on
log_line_prefix = '%m [%p] %q%u@%d '
log_min_messages = warning
EOF
  if [ -n "$extra" ]; then printf '%s\n' "$extra" >>"${d}/data/postgresql.conf"; fi
  chown postgres:postgres "${d}/data/pg_hba.conf" "${d}/data/postgresql.conf"
  DRILL_CLUSTERS+=("$name")
}

cl_dir()  { printf '%s' "${DRILL_ROOT}/$1"; }
cl_sock() { printf '%s' "${DRILL_ROOT}/$1/sock"; }

cl_start() { # name port
  local name="$1" d
  d="$(cl_dir "$name")"
  pgas "${PG_BIN}/pg_ctl" -D "${d}/data" -l "${d}/server.log" -w -t 90 start >"${d}/pg_ctl.start.log" 2>&1 \
    || { tail -20 "${d}/server.log" >&2; bk_die "pg_ctl start failed for ${name}"; }
}

cl_stop() { # name  (idempotent)
  local name="$1" d
  d="$(cl_dir "$name")"
  if [ -f "${d}/data/postmaster.pid" ]; then
    pgas "${PG_BIN}/pg_ctl" -D "${d}/data" -m fast -w -t 120 stop >/dev/null 2>&1 || true
  fi
}

# cl_super name port <psql args...> : superuser of the THROWAWAY cluster over its private unix socket
# (trust auth; the socket directory is 0700 and postgres-owned).
cl_super() {
  local name="$1" port="$2"; shift 2
  psql -X -q -At -v ON_ERROR_STOP=1 -h "$(cl_sock "$name")" -p "$port" -U postgres "$@"
}

# cl_write_pass user pass port file : append one passfile line for 127.0.0.1:port (any database).
cl_write_pass() {
  local user="$1" pass="$2" port="$3" file="$4"
  touch "$file"; chmod 600 "$file"
  printf '127.0.0.1:%s:*:%s:%s\n' "$port" "$user" "$pass" >>"$file"
}

cl_random_secret() { head -c 36 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32; }

cl_teardown() {
  local rc=$? n i
  set +e
  # reverse start order
  for ((i=${#DRILL_CLUSTERS[@]}-1; i>=0; i--)); do cl_stop "${DRILL_CLUSTERS[$i]}"; done
  if [ -n "$DRILL_ROOT" ]; then
    case "$DRILL_ROOT" in
      /tmp/pgdrill-??????) if [ "${DRILL_KEEP:-0}" = 1 ]; then echo "backup: DRILL_KEEP=1, leaving ${DRILL_ROOT}"; else rm -rf "$DRILL_ROOT"; fi ;;
      *) echo "backup: refusing to remove unexpected root '${DRILL_ROOT}'" >&2 ;;
    esac
  fi
  exit "$rc"
}
