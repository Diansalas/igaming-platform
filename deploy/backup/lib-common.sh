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
#   BACKUP_PGSSLMODE   libpq sslmode (default: prefer for local drill targets, require otherwise)
#
# Target guard (fail closed, A12 security review M5). The host is CANONICALISED before any decision: lower-cased, brackets and
# trailing dots stripped, multi-host lists and unix-socket paths rejected, and RESOLVED with the system resolver
# (`getent ahosts`), so 127.1, 127.0.0.2, 0, 0.0.0.0, ::ffff:127.0.0.1, 0:0:0:0:0:0:0:1, LOCALHOST, localhost., [::1] and this
# machine's own host name are all recognised as THIS machine. A target that resolves to this machine must use one of the
# throwaway drill ports (BACKUP_DRILL_PORTS, default "5499 5498 5497"); the shared development/CI cluster (5432) and every other
# local port are refused with NO acknowledgement possible. Only a target that does not resolve to this machine (the staging RDS
# instance, later) can be allowed, and only per invocation: BACKUP_ALLOW_REMOTE_TARGET must equal the canonical host. Production
# is not authorised.

# shellcheck shell=bash

bk_die() { echo "backup: $*" >&2; exit 2; }
bk_log() { echo "backup: $*"; }

# libpq must see only what we set below.
unset PGHOST PGHOSTADDR PGPORT PGSERVICE PGSERVICEFILE PGSYSCONFDIR PGOPTIONS PGTARGETSESSIONATTRS \
  PGDATABASE PGUSER PGPASSWORD PGPASSFILE PGSSLMODE PGSSLROOTCERT PGCONNECT_TIMEOUT

BK_TARGET_LOCAL=0
BK_CANON_HOST=""

# bk_canonical_host <raw host> -> prints the canonical form or returns 1 for forms that are refused outright
bk_canonical_host() {
  local h="$1"
  [ -n "$h" ] || return 1
  case "$h" in *,*|*[[:space:]]*|*@*|*/*|*\\*|*%*) return 1 ;; esac   # multi-host lists, whitespace, userinfo, paths, scope ids
  h="${h,,}"
  case "$h" in \[*\]) h="${h#\[}"; h="${h%\]}" ;; esac
  while [ "${h%.}" != "$h" ]; do h="${h%.}"; done
  [ -n "$h" ] || return 1
  printf '%s' "$h"
}

# bk_local_addresses : addresses assigned to this machine, one per line
bk_local_addresses() {
  { ip -o addr show 2>/dev/null | awk '{print $4}' | sed 's|/.*||'; hostname -I 2>/dev/null | tr ' ' '\n'; } | sed '/^$/d'
}

# bk_addr_is_local <address> : loopback / unspecified / mapped-loopback / assigned to this machine
bk_addr_is_local() {
  local a="${1,,}"
  case "$a" in
    127.*|0.0.0.0|::|::1|::ffff:127.*|::ffff:0.0.0.0|::ffff:7f*) return 0 ;;
  esac
  # fully written IPv6 forms of ::, ::1 and mapped loopback (0:0:0:0:0:0:0:1, 0000:...:0001, 0:0:0:0:0:ffff:7f00:1)
  local re_zero='^(0{1,4}:){7}0{1,4}$' re_one='^(0{1,4}:){7}0{0,3}1$' re_map='^(0{1,4}:){5}ffff:(7f|0{1,4}:)'
  [[ "$a" =~ $re_zero || "$a" =~ $re_one || "$a" =~ $re_map ]] && return 0
  bk_local_addresses | tr 'A-Z' 'a-z' | grep -qxF -- "$a"
}

# bk_host_is_local <canonical host> : 0 if the name or literal resolves to this machine (any of its addresses), 1 otherwise.
# Unresolvable names are treated as REMOTE (a connection to them cannot reach this machine either).
bk_host_is_local() {
  local h="$1" a
  case "$h" in localhost|*.localhost) return 0 ;; esac
  # IPv6 literals are judged on their own (the resolver may not parse them): anything not clearly this machine is not accepted
  case "$h" in *:*) bk_addr_is_local "$h"; return ;; esac
  command -v getent >/dev/null 2>&1 || { [ "$h" = 127.0.0.1 ]; return; }
  while read -r a _; do
    [ -n "$a" ] || continue
    bk_addr_is_local "$a" && return 0
  done < <(getent ahosts "$h" 2>/dev/null)
  return 1
}

bk_validate_ident() { # what value
  [[ "$2" =~ ^[a-z][a-z0-9_]{0,62}$ ]] || bk_die "$1 must match ^[a-z][a-z0-9_]{0,62}\$ (got '${2:0:40}')"
}

# bk_guard_target host port : refuse the shared cluster and anything not explicitly allowed. Sets BK_CANON_HOST and BK_TARGET_LOCAL
# (1 = a throwaway drill instance on this machine).
bk_guard_target() {
  local raw="$1" port="$2" host p ok=0
  [ -n "$raw" ] || bk_die "BACKUP_PGHOST is required"
  case "$port" in ''|*[!0-9]*) bk_die "BACKUP_PGPORT must be an integer" ;; esac
  host="$(bk_canonical_host "$raw")" || bk_die "host '${raw:0:60}' refused: empty, multi-host list, unix-socket path, userinfo or scope id are not accepted"
  BK_CANON_HOST="$host"
  if bk_host_is_local "$host"; then
    BK_TARGET_LOCAL=1
    [ "$port" != 5432 ] || bk_die "refusing ${raw}:5432: it resolves to THIS machine and 5432 is the SHARED local cluster used by all tests (CLAUDE.md environment safety). Use a throwaway drill cluster (deploy/backup/drill.sh). No acknowledgement can override this"
    for p in ${BACKUP_DRILL_PORTS:-5499 5498 5497}; do [ "$p" = "$port" ] && ok=1; done
    [ "$ok" -eq 1 ] || bk_die "${raw}:${port} resolves to this machine and is not a throwaway drill port (${BACKUP_DRILL_PORTS:-5499 5498 5497}); refusing"
    BK_CANON_HOST=127.0.0.1
  else
    BK_TARGET_LOCAL=0
    [ "$(bk_canonical_host "${BACKUP_ALLOW_REMOTE_TARGET:-}" 2>/dev/null || true)" = "$host" ] \
      || bk_die "remote host '${host}' refused: set BACKUP_ALLOW_REMOTE_TARGET=${host} to acknowledge this is the intended (staging) target. Production is not authorised."
  fi
}

# bk_connect_env : validate BACKUP_* and export the libpq environment for child processes.
bk_connect_env() {
  : "${BACKUP_PGHOST:?BACKUP_PGHOST is required}"
  : "${BACKUP_PGPORT:?BACKUP_PGPORT is required}"
  : "${BACKUP_PGDATABASE:?BACKUP_PGDATABASE is required}"
  : "${BACKUP_PGUSER:?BACKUP_PGUSER is required}"
  bk_validate_ident BACKUP_PGDATABASE "$BACKUP_PGDATABASE"
  bk_validate_ident BACKUP_PGUSER "$BACKUP_PGUSER"
  bk_guard_target "$BACKUP_PGHOST" "$BACKUP_PGPORT"
  local pf="${BACKUP_PGPASSFILE:-}"
  [ -n "$pf" ] || bk_die "BACKUP_PGPASSFILE is required (path to a 0600 libpq passfile materialised from the secret store; no password is accepted in the environment, argv or a URL)"
  [ -f "$pf" ] || bk_die "credential file missing: ${pf} (the backup credential was not provisioned, or it expired and was removed)"
  local mode
  mode="$(stat -c %a "$pf")"
  case "$mode" in 600|400) ;; *) bk_die "credential file ${pf} must be mode 0600 or 0400 (is ${mode})" ;; esac
  export PGHOST="$BK_CANON_HOST" PGPORT="$BACKUP_PGPORT" PGDATABASE="$BACKUP_PGDATABASE" PGUSER="$BACKUP_PGUSER" PGPASSFILE="$pf"
  export PGCONNECT_TIMEOUT=10
  if [ "$BK_TARGET_LOCAL" -eq 1 ]; then export PGSSLMODE="${BACKUP_PGSSLMODE:-prefer}"; else export PGSSLMODE="${BACKUP_PGSSLMODE:-require}"; fi
}

bk_need_tools() { local t; for t in "$@"; do command -v "$t" >/dev/null 2>&1 || bk_die "required tool not found: $t"; done; }

# bk_mktemp_dir <prefix> : a private (0700) directory under the CONTROLLED temp root (BACKUP_TMPDIR, default TMPDIR, default /tmp)
# with an unpredictable name; the owning pid is recorded so a later run can sweep it if this process was killed (bk_sweep_stale).
bk_mktemp_dir() {
  local root="${BACKUP_TMPDIR:-${TMPDIR:-/tmp}}" d
  mkdir -p "$root" && chmod 700 "$root" 2>/dev/null || true
  d="$(umask 077; mktemp -d "${root}/bk-$1.XXXXXXXX")" || bk_die "cannot create a temporary directory under ${root}"
  printf '%s' "$$" >"${d}/.pid"
  printf '%s' "$d"
}

# bk_sweep_stale : remove temp directories of runs that no longer exist (killed with SIGKILL / OOM)
bk_sweep_stale() {
  local root="${BACKUP_TMPDIR:-${TMPDIR:-/tmp}}" d pid
  for d in "${root}"/bk-*.????????; do
    [ -d "$d" ] && [ -f "${d}/.pid" ] || continue
    pid="$(cat "${d}/.pid" 2>/dev/null || true)"
    if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then rm -rf "$d"; bk_log "swept stale temporary directory ${d}"; fi
  done
}

# bk_kill_tree <pid> : terminate a process and all its descendants (children first; no pkill, no process-name matching)
bk_kill_tree() {
  local p="$1" c
  for c in $(ps -o pid= --ppid "$p" 2>/dev/null); do bk_kill_tree "$c"; done
  kill "$p" 2>/dev/null || true
}

# bk_sql <sql> : run one statement against the connected target, tuples only.
bk_sql() { psql -X -q -At -v ON_ERROR_STOP=1 -c "$1"; }

now_s() { date +%s.%N; }
elapsed_s() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }
