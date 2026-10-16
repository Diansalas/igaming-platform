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
# Refusals (exit 2): no --retired-kid (unless --first-key); a non-drill target without sslmode=verify-full; a secret file that is not a regular 0600 file owned by the caller (or not on tmpfs
# unless a drill target); a kid reused from --retired-kid or already present; ANY existing key (this is a first-key provisioning of a restored database;
# rotation of a live key is operational-runbooks.md section 15, not this script); statement logging that could capture the bound secret
# (log_statement mod/all, log_min_duration_statement >= 0, a non-zero log_parameter_max_length_on_error, a non-zero log_transaction_sample_rate / sampled statement logging, auto_explain parameter logging, or pgaudit with log_parameter on); a connecting role that is not the table owner.
# On success it prints the kid, status and secret LENGTH only, and that the SECURITY DEFINER probe reports the kid active.
#
# After it: set ACTOR_PROOF_KEYS / ACTOR_PROOF_ACTIVE_KID in the secret store to the new pair (remove the old pair), then
# deploy/backup/verify.sh --expect-actor-proof provisioned --actor-proof-kid <kid> --retired-kid <old kid>.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"

KID=""; SECRET_FILE=""; RETIRED=(); FIRST_KEY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --kid) shift; KID="${1:-}" ;;
    --secret-hex-file) shift; SECRET_FILE="${1:-}" ;;
    --retired-kid) shift; RETIRED+=("${1:-}") ;;
    --first-key) FIRST_KEY=1 ;;
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
# the source's key ids must be named (they are in the set's baseline.txt as META|actor_proof_kids|, or the application's previous ACTOR_PROOF_ACTIVE_KID), unless this is a brand-new environment
if [ "${#RETIRED[@]}" -eq 0 ] && [ "$FIRST_KEY" -ne 1 ]; then
  bk_die "name the source's key id(s) with --retired-kid (from baseline.txt META|actor_proof_kids| or the old ACTOR_PROOF_ACTIVE_KID) so the new kid cannot repeat one, or pass --first-key if there never was a key"
fi
[ "${#RETIRED[@]}" -eq 0 ] || [ "$FIRST_KEY" -ne 1 ] || bk_die "--first-key and --retired-kid are mutually exclusive"

# the connection is made by REKEY_* (the owner role), validated exactly like the backup tools' BACKUP_* target (drill ports only on this machine)
: "${REKEY_PGHOST:?REKEY_PGHOST is required}" "${REKEY_PGPORT:?REKEY_PGPORT is required}" "${REKEY_PGDATABASE:?REKEY_PGDATABASE is required}" "${REKEY_PGUSER:?REKEY_PGUSER is required}"
BACKUP_PGHOST="$REKEY_PGHOST" BACKUP_PGPORT="$REKEY_PGPORT" BACKUP_PGDATABASE="$REKEY_PGDATABASE" BACKUP_PGUSER="$REKEY_PGUSER" BACKUP_PGPASSFILE="${REKEY_PGPASSFILE:-}"
export BACKUP_PGHOST BACKUP_PGPORT BACKUP_PGDATABASE BACKUP_PGUSER BACKUP_PGPASSFILE
bk_need_tools psql stat
PSQL_MAJOR="$(psql --version | sed -n 's/^psql (PostgreSQL) \([0-9]*\).*/\1/p')"
[ "${PSQL_MAJOR:-0}" -ge 16 ] || bk_die "psql 16 or later is required (\\bind sends the secret as a bound parameter; older clients would need it in statement text)"
bk_connect_env
umask 077

# the secret file is opened ONCE (descriptor 9); every check below is made on that open file (not on the path again) and the bytes are read once into a variable.
# A swap of the path after this point cannot change what is validated or inserted.
{ [ -f "$SECRET_FILE" ] && [ ! -L "$SECRET_FILE" ]; } || bk_die "--secret-hex-file must be a regular file (not a symlink)"
exec 9<"$SECRET_FILE" || bk_die "cannot open --secret-hex-file"
[ "$(stat -L -c %F /proc/self/fd/9)" = "regular file" ] || bk_die "--secret-hex-file must be a regular file"
case "$(stat -L -c %a /proc/self/fd/9)" in 600|400) ;; *) bk_die "--secret-hex-file must be mode 0600 or 0400" ;; esac
[ "$(stat -L -c %u /proc/self/fd/9)" = "$(id -u)" ] || bk_die "--secret-hex-file must be owned by the invoking user"
if [ "$BK_TARGET_LOCAL" -ne 1 ]; then
  case "$(stat -L -f -c %T /proc/self/fd/9)" in tmpfs|ramfs) ;; *) bk_die "--secret-hex-file must be on tmpfs (e.g. /dev/shm) for a real target: the secret must never reach a disk" ;; esac
fi
AP_SECRET_HEX="$(tr -d '\n' <&9)"
exec 9<&-
[[ "$AP_SECRET_HEX" =~ ^([0-9a-fA-F]{2}){32,256}$ ]] || bk_die "--secret-hex-file must hold one even-length hex string of 32..256 bytes (64..512 hex digits)"
export AP_SECRET_HEX

# connection-level preconditions (no secret involved yet)
PRE="$(psql -X -q -At -F '|' -v ON_ERROR_STOP=1 -c "SELECT (c.relowner = r.oid)::int, current_setting('log_statement'), current_setting('log_min_duration_statement'), coalesce(current_setting('log_parameter_max_length_on_error', true), '0'), (current_setting('log_transaction_sample_rate')::float8 <> 0)::int, (current_setting('log_statement_sample_rate')::float8 <> 0 AND current_setting('log_min_duration_sample') <> '-1')::int, (coalesce(current_setting('auto_explain.log_min_duration', true), '-1') <> '-1' AND coalesce(current_setting('auto_explain.log_parameter_max_length', true), '-1') <> '0')::int, (coalesce(nullif(current_setting('pgaudit.log', true), ''), 'none') <> 'none' AND coalesce(nullif(current_setting('pgaudit.log_parameter', true), ''), 'off') IN ('on', 'true', '1'))::int, (SELECT count(*) FROM actorproof_private.actor_proof_keys), (SELECT count(*) FROM actorproof_private.actor_proof_keys WHERE kid = '${KID}') FROM pg_class c, pg_roles r WHERE c.oid = 'actorproof_private.actor_proof_keys'::regclass AND r.rolname = current_user" 2>/dev/null)" \
  || bk_die "cannot read actorproof_private.actor_proof_keys as ${REKEY_PGUSER}: it must be the migration/owner role of a database with migrations 0120 and 0166 applied"
IFS='|' read -r IS_OWNER LOGSTMT LOGDUR LOGPARAM_ERR LOGTXN LOGSAMPLE AUTOEXP PGAUDIT NKEYS HAS_KID <<<"$PRE"
[ "$IS_OWNER" = 1 ] || bk_die "${REKEY_PGUSER} does not own actor_proof_keys; only the owner role provisions keys (operational-runbooks.md section 15)"
[ "$HAS_KID" = 0 ] || bk_die "kid '${KID}' is already present in actor_proof_keys"
[ "$NKEYS" = 0 ] || bk_die "actor_proof_keys already holds ${NKEYS} key(s): this script provisions the FIRST key of a restored database. Rotating a live key is operational-runbooks.md section 15 (rotation)"
case "$LOGSTMT" in mod|all) bk_die "log_statement=${LOGSTMT}: the server could log the statement; set it to none/ddl for this session's role (runbook section 15 step 4). Nothing was written" ;; esac
[ "$LOGDUR" = -1 ] || bk_die "log_min_duration_statement=${LOGDUR}: a slow/any statement could be logged with its parameters; it must be -1 (runbook section 15 step 4). Nothing was written"
[ "$LOGPARAM_ERR" = 0 ] || bk_die "log_parameter_max_length_on_error=${LOGPARAM_ERR}: an error would log the bound secret; it must be 0. Nothing was written"

[ "$LOGTXN" = 0 ] || bk_die "log_transaction_sample_rate is non-zero: a sampled transaction logs every statement WITH its bound parameters; set it to 0 for this role. Nothing was written"
[ "$LOGSAMPLE" = 0 ] || bk_die "log_statement_sample_rate with log_min_duration_sample could log the statement with its parameters; set log_statement_sample_rate=0 (or log_min_duration_sample=-1). Nothing was written"
[ "$AUTOEXP" = 0 ] || bk_die "auto_explain logs parameters (auto_explain.log_min_duration set and auto_explain.log_parameter_max_length not 0); disable it for this role. Nothing was written"
[ "$PGAUDIT" = 0 ] || bk_die "pgaudit is active with pgaudit.log_parameter=on: the bound secret would be written to the audit log (CloudWatch on RDS); set pgaudit.log_parameter=off for this role/session. Nothing was written"

# the INSERT (atomic: the table is locked SHARE ROW EXCLUSIVE and the row is inserted only if the table is still EMPTY, so two concurrent runs cannot both provision): the secret travels as a bound PARAMETER (extended protocol), never in statement text and never in argv (psql reads the file itself)
psql -X -q -At -v ON_ERROR_STOP=1 -v ap_kid="$KID" <<'SQL' >/dev/null
\getenv ap_hex AP_SECRET_HEX
BEGIN;
LOCK TABLE actorproof_private.actor_proof_keys IN SHARE ROW EXCLUSIVE MODE;
INSERT INTO actorproof_private.actor_proof_keys (kid, secret) SELECT $1, decode($2, 'hex') WHERE NOT EXISTS (SELECT 1 FROM actorproof_private.actor_proof_keys) \bind :ap_kid :ap_hex \g
COMMIT;
SQL
unset AP_SECRET_HEX
[ "$(psql -X -q -At -c "SELECT count(*) FROM actorproof_private.actor_proof_keys WHERE kid = '${KID}'")" = 1 ] || bk_die "the key was NOT inserted: another key appeared between the check and the insert (nothing was changed)"
bk_log "actor-proof key '${KID}' provisioned: $(psql -X -q -At -F ', ' -v ON_ERROR_STOP=1 -c "SELECT 'status=' || status || ', secret_bytes=' || octet_length(secret) || ', probe_active=' || actor_proof_key_active(kid)::text FROM actorproof_private.actor_proof_keys WHERE kid = '${KID}'")"
echo "rekey: NEXT: put ${KID} (and the same secret as kid:base64) into the application's ACTOR_PROOF_ACTIVE_KID / ACTOR_PROOF_KEYS in the secret store, remove the old pair, shred ${SECRET_FILE##*/}, then run verify.sh --expect-actor-proof provisioned."
