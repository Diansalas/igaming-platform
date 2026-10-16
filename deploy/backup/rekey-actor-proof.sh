#!/usr/bin/env bash
# Provision a NEW actor-proof verification key into a RESTORED database (owner decision 5, ADR 0122 section 10; ADR 0110).
#
# WHY. A logical backup never carries actor_proof_keys.secret, so a database restored from one has NO actor-proof key: every governed write (K1/K2/K3,
# launch decisions, RG floors, staff lifecycle) is refused with AP002 and a production platform-api refuses to start. This script is the scripted
# form of the reviewed manual procedure in docs/runbooks/operational-runbooks.md section 15 step 4 (owner role, parameter-bound INSERT, secret read from
# a 0600 file, statement logging checked first). It adds nothing to that procedure except refusals. It does NOT generate, store or print a secret and it
# never copies a key out of any database or backup.
#
#   REKEY_PGHOST=127.0.0.1 REKEY_PGPORT=5498 REKEY_PGDATABASE=igaming_platform_dev REKEY_PGUSER=igaming REKEY_PGPASSFILE=/run/secrets/owner.pgpass \
#   deploy/backup/rekey-actor-proof.sh --kid ap-2026-11 --secret-hex-file /dev/shm/ap-2026-11.hex [--retired-kid ap-2026-10]...
#
#   --kid                 the NEW key id ([A-Za-z0-9._-]{1,32}); it MUST equal the application's ACTOR_PROOF_ACTIVE_KID
#   --secret-hex-file     a mode-0600 file (on tmpfs) holding the hex of the new secret (>= 32 bytes); the SAME secret goes into the application's
#                         ACTOR_PROOF_KEYS as base64 (kid:base64) from the secret store. Generate it in the secret store, never in this repository
#   --retired-kid         the key id(s) of the SOURCE (printed by backup.sh in baseline.txt as `META|actor_proof_kids|...`, or from the application's old
#                         ACTOR_PROOF_KEYS): the new kid must differ from every one of them. The old key stays retired by absence: the restored table never had it
#
# Refusals (exit 2): a non-drill target without sslmode=verify-full; a secret file that is not a regular 0600 file owned by the caller (or not on tmpfs
# unless a drill target); a kid reused from --retired-kid or already present; ANY existing key (this is a first-key provisioning of a restored database;
# rotation of a live key is operational-runbooks.md section 15, not this script); statement logging that could capture the bound secret
# (log_statement mod/all, log_min_duration_statement >= 0, or a non-zero log_parameter_max_length_on_error); a connecting role that is not the table owner.
# On success it prints the kid, status and secret LENGTH only, and that the SECURITY DEFINER probe reports the kid active.
#
# After it: set ACTOR_PROOF_KEYS / ACTOR_PROOF_ACTIVE_KID in the secret store to the new pair (remove the old pair), then
# deploy/backup/verify.sh --expect-actor-proof provisioned --actor-proof-kid <kid> --retired-kid <old kid>.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"

KID=""; SECRET_FILE=""; RETIRED=()
while [ $# -gt 0 ]; do
  case "$1" in
    --kid) shift; KID="${1:-}" ;;
    --secret-hex-file) shift; SECRET_FILE="${1:-}" ;;
    --retired-kid) shift; RETIRED+=("${1:-}") ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 0 ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done
[[ "$KID" =~ ^[A-Za-z0-9._-]{1,32}$ ]] || bk_die "--kid must match [A-Za-z0-9._-]{1,32}"
for k in "${RETIRED[@]}"; do
  [[ "$k" =~ ^[A-Za-z0-9._-]{1,32}$ ]] || bk_die "--retired-kid must match [A-Za-z0-9._-]{1,32}"
  [ "$k" != "$KID" ] || bk_die "the new kid '${KID}' equals a retired kid: a restored platform must NOT reuse the source's key id (provision a different one)"
done
[ -n "$SECRET_FILE" ] || bk_die "--secret-hex-file is required"

# the connection is made by REKEY_* (the owner role), validated exactly like the backup tools' BACKUP_* target (drill ports only on this machine)
: "${REKEY_PGHOST:?REKEY_PGHOST is required}" "${REKEY_PGPORT:?REKEY_PGPORT is required}" "${REKEY_PGDATABASE:?REKEY_PGDATABASE is required}" "${REKEY_PGUSER:?REKEY_PGUSER is required}"
BACKUP_PGHOST="$REKEY_PGHOST" BACKUP_PGPORT="$REKEY_PGPORT" BACKUP_PGDATABASE="$REKEY_PGDATABASE" BACKUP_PGUSER="$REKEY_PGUSER" BACKUP_PGPASSFILE="${REKEY_PGPASSFILE:-}"
export BACKUP_PGHOST BACKUP_PGPORT BACKUP_PGDATABASE BACKUP_PGUSER BACKUP_PGPASSFILE
bk_need_tools psql stat
PSQL_MAJOR="$(psql --version | sed -n 's/^psql (PostgreSQL) \([0-9]*\).*/\1/p')"
[ "${PSQL_MAJOR:-0}" -ge 16 ] || bk_die "psql 16 or later is required (\\bind sends the secret as a bound parameter; older clients would need it in statement text)"
bk_connect_env
umask 077

# the secret file: regular, not a symlink, mode 0600/0400, owned by the caller, tmpfs (RAM) unless this is a throwaway drill target
{ [ -f "$SECRET_FILE" ] && [ ! -L "$SECRET_FILE" ]; } || bk_die "--secret-hex-file must be a regular file (not a symlink)"
case "$(stat -c %a "$SECRET_FILE")" in 600|400) ;; *) bk_die "--secret-hex-file must be mode 0600 or 0400" ;; esac
[ "$(stat -c %u "$SECRET_FILE")" = "$(id -u)" ] || bk_die "--secret-hex-file must be owned by the invoking user"
if [ "$BK_TARGET_LOCAL" -ne 1 ]; then
  case "$(stat -f -c %T "$SECRET_FILE")" in tmpfs|ramfs) ;; *) bk_die "--secret-hex-file must be on tmpfs (e.g. /dev/shm) for a real target: the secret must never reach a disk" ;; esac
fi
HEXLEN="$(tr -d '\n' <"$SECRET_FILE" | wc -c)"
if [ "$HEXLEN" -lt 64 ] || [ $((HEXLEN % 2)) -ne 0 ] || [ "$HEXLEN" -gt 512 ] || tr -d '\n' <"$SECRET_FILE" | grep -qE '[^0-9a-fA-F]'; then
  bk_die "--secret-hex-file must hold one even-length hex string of 32..256 bytes (64..512 hex digits)"
fi

# connection-level preconditions (no secret involved yet)
PRE="$(psql -X -q -At -F '|' -v ON_ERROR_STOP=1 -c "SELECT (c.relowner = r.oid)::int, current_setting('log_statement'), current_setting('log_min_duration_statement'), coalesce(current_setting('log_parameter_max_length_on_error', true), '0'), (SELECT count(*) FROM public.actor_proof_keys), (SELECT count(*) FROM public.actor_proof_keys WHERE kid = '${KID}') FROM pg_class c, pg_roles r WHERE c.oid = 'public.actor_proof_keys'::regclass AND r.rolname = current_user" 2>/dev/null)" \
  || bk_die "cannot read public.actor_proof_keys as ${REKEY_PGUSER}: it must be the migration/owner role of a database with migration 0120 applied"
IFS='|' read -r IS_OWNER LOGSTMT LOGDUR LOGPARAM_ERR NKEYS HAS_KID <<<"$PRE"
[ "$IS_OWNER" = 1 ] || bk_die "${REKEY_PGUSER} does not own actor_proof_keys; only the owner role provisions keys (operational-runbooks.md section 15)"
[ "$HAS_KID" = 0 ] || bk_die "kid '${KID}' is already present in actor_proof_keys"
[ "$NKEYS" = 0 ] || bk_die "actor_proof_keys already holds ${NKEYS} key(s): this script provisions the FIRST key of a restored database. Rotating a live key is operational-runbooks.md section 15 (rotation)"
case "$LOGSTMT" in mod|all) bk_die "log_statement=${LOGSTMT}: the server could log the statement; set it to none/ddl for this session's role (runbook section 15 step 4). Nothing was written" ;; esac
[ "$LOGDUR" = -1 ] || bk_die "log_min_duration_statement=${LOGDUR}: a slow/any statement could be logged with its parameters; it must be -1 (runbook section 15 step 4). Nothing was written"
[ "$LOGPARAM_ERR" = 0 ] || bk_die "log_parameter_max_length_on_error=${LOGPARAM_ERR}: an error would log the bound secret; it must be 0. Nothing was written"

# the INSERT: the secret travels as a bound PARAMETER (extended protocol), never in statement text and never in argv (psql reads the file itself)
AP_SECRET_FILE="$SECRET_FILE" psql -X -q -At -v ON_ERROR_STOP=1 -v ap_kid="$KID" <<'SQL' >/dev/null
\set ap_hex `cat "$AP_SECRET_FILE"`
INSERT INTO public.actor_proof_keys (kid, secret) VALUES ($1, decode($2, 'hex')) \bind :ap_kid :ap_hex \g
SQL
bk_log "actor-proof key '${KID}' provisioned: $(psql -X -q -At -F ', ' -v ON_ERROR_STOP=1 -c "SELECT 'status=' || status || ', secret_bytes=' || octet_length(secret) || ', probe_active=' || actor_proof_key_active(kid)::text FROM public.actor_proof_keys WHERE kid = '${KID}'")"
echo "rekey: NEXT: put ${KID} (and the same secret as kid:base64) into the application's ACTOR_PROOF_ACTIVE_KID / ACTOR_PROOF_KEYS in the secret store, remove the old pair, shred ${SECRET_FILE##*/}, then run verify.sh --expect-actor-proof provisioned."
