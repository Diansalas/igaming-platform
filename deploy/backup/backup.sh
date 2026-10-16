#!/usr/bin/env bash
# Logical backup of the platform database as the dedicated, audited backup role (ADR 0122, BACKUP-ROLE-1).
#
#   BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT=5499 BACKUP_PGDATABASE=igaming_platform_dev BACKUP_PGUSER=igaming_backup \
#   BACKUP_PGPASSFILE=/run/secrets/backup.pgpass BACKUP_SIGNING_KEY_FILE=/run/secrets/backup-sign.pem \
#   BACKUP_ENCRYPT_AGE_RECIPIENT=age1... deploy/backup/backup.sh --out-dir /var/backups/igaming
#
# What it produces (one directory per run, mode 0700, files 0600):
#   db.dump.age|.gpg        pg_dump -Fc of the whole database, taken in ONE exported REPEATABLE READ snapshot, STREAMED through the
#                           encryptor: the plaintext dump never touches the disk
#   globals.sql.age|.gpg    pg_dumpall --globals-only --no-role-passwords (role attributes/grants; NO passwords)
#   baseline.txt.age|.gpg   baseline.sql evaluated in the SAME snapshot (exact row counts + content hashes per table, ledger
#                           invariants, sequences, roles/grants, triggers, policies, catalog fingerprints)
#   manifest.json           what/when/where, tool versions, sizes, timings, invariant status. No secret.
#
# SIGNING MATERIAL IS NOT IN A LOGICAL DUMP, AND THE BACKUP ROLE CANNOT READ IT (owner decision 5, ADR 0122 section 10; F1 / migration 0166):
# the actor-proof HMAC secrets live in the schema actorproof_private, on which this role holds NO privilege (no USAGE, no table grant; checked
# in the preflight below, which refuses the backup otherwise). pg_dump runs with --exclude-schema=actorproof_private, so it neither locks nor
# reads that table. The table of contents of the stream is read while it is written and the backup FAILS if any entry of that schema or any
# actor_proof_key* data entry (in ANY schema) is present; the manifest records the derived fact (`actor_proof_keys_data`: excluded). A
# restored database therefore starts with an EMPTY key table (restore.sh recreates the schema from actorproof-private-schema.sql) and every
# governed write is refused (AP002) until deploy/backup/rekey-actor-proof.sh provisions a NEW key. Physical base backups / PITR are not
# affected (they stay inside the cloud trust boundary and carry the live key).
#   SHA256SUMS              sha256 of every artefact above INCLUDING manifest.json
#   SHA256SUMS.sig          Ed25519 signature over SHA256SUMS (key by reference: BACKUP_SIGNING_KEY_FILE). restore.sh refuses a set
#                           that is unsigned or not signed by the pinned key
#
# Fail-closed behaviour (A12 security review H1-H3, L1-L8):
#   * the connecting role must be NOSUPERUSER + BYPASSRLS + none of CREATEROLE/CREATEDB/REPLICATION, read-only by default, able to
#     SELECT every table (BACKUP-ROLE-1: any other role gives a refused or silently incomplete dump);
#   * ENCRYPTION IS REQUIRED and is validated BEFORE anything is dumped: BACKUP_ENCRYPT_AGE_RECIPIENT (an age recipient; a test
#     encryption must succeed) or BACKUP_ENCRYPT_GPG_RECIPIENT (a full 40-hex key fingerprint present in the keyring; a test encryption
#     must succeed). A failing encryptor fails the whole backup (pipefail) and the run directory is removed; afterwards the script
#     asserts that no plaintext artefact exists. The only exception is the synthetic-data override BACKUP_ALLOW_UNENCRYPTED=synthetic-
#     data-only, honoured ONLY for a throwaway drill target on this machine;
#   * the manifest's `encryption` field is derived from the files actually written (magic bytes), never from configuration;
#   * the snapshot-holder session is kept alive by a keepalive query (the role's idle_in_transaction_session_timeout would otherwise
#     kill it after 30 minutes) and is monitored: if it dies the whole backup fails loudly;
#   * SIGINT/SIGTERM/SIGHUP/EXIT clean up; SIGPIPE is ignored and handled as an error; temporary files live in a 0700 directory
#     under BACKUP_TMPDIR; stale temporaries and orphaned incomplete run directories of killed runs are swept at start;
#   * source ledger/projection invariants are checked on the snapshot; a violation is recorded and the script exits 4 (the
#     artefacts are kept and marked, they are evidence).
#
# Exit: 0 ok; 2 refused/usage/preflight; 3 backup failed; 4 backup written but SOURCE INVARIANTS VIOLATED.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"
# shellcheck source=deploy/backup/lib-sign.sh
. "${SCRIPT_DIR}/lib-sign.sh"

OUT_DIR=""
LABEL=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out-dir) shift; OUT_DIR="${1:-}" ;;
    --label) shift; LABEL="${1:-}" ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 0 ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done
[ -n "$OUT_DIR" ] || bk_die "--out-dir is required"
case "$LABEL" in ''|*[!A-Za-z0-9_.-]*) [ -z "$LABEL" ] || bk_die "--label may contain only A-Za-z0-9_.-" ;; esac

bk_need_tools psql pg_dump pg_dumpall sha256sum awk openssl mktemp
bk_connect_env
umask 077
# the unencrypted synthetic-data override is decided BEFORE any connection and honoured only for a local throwaway target (L5)
if [ -z "${BACKUP_ENCRYPT_AGE_RECIPIENT:-}${BACKUP_ENCRYPT_GPG_RECIPIENT:-}" ]; then
  [ "${BACKUP_ALLOW_UNENCRYPTED:-}" = "synthetic-data-only" ] && [ "$BK_TARGET_LOCAL" -eq 1 ] \
    || bk_die "no encryption recipient configured. A real dump contains PII, financial data and signing keys: set BACKUP_ENCRYPT_AGE_RECIPIENT or BACKUP_ENCRYPT_GPG_RECIPIENT (public references only). The override BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only is honoured only for a throwaway drill target on this machine"
fi

# ---- signals, cleanup ------------------------------------------------------------------------------------------------
TMPW=""; RUN_DIR=""; SESSION_PID=""; KEEP_PID=""; DRAIN_PID=""; DUMP_PID=""; SLEEP_PID=""
trap '' PIPE                       # a write to a dead pipe must be an error we handle, not a silent death without cleanup
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
cleanup() {
  local rc=$?
  set +e
  [ -n "$DUMP_PID" ] && bk_kill_tree "$DUMP_PID"   # the whole pipeline: pg_dump, tee, the encryptor
  for p in "$KEEP_PID" "$DRAIN_PID" "$SESSION_PID" "$SLEEP_PID"; do [ -n "$p" ] && bk_kill_tree "$p"; done
  wait 2>/dev/null   # reap them: a background writer (the keepalive's holder.dead marker) must not race the removal below
  exec 7>&- 8<&-
  if [ -n "$TMPW" ]; then rm -rf "$TMPW" 2>/dev/null || { sleep 0.5; rm -rf "$TMPW"; }; fi
  if [ -n "$RUN_DIR" ]; then
    if [ "$rc" -ne 0 ] && [ "$rc" -ne 4 ]; then
      rm -rf "$RUN_DIR"
      echo "backup: FAILED (exit ${rc}); partial output ${RUN_DIR} removed" >&2
    else
      rm -f "${RUN_DIR}/.incomplete"
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT

bk_sweep_stale
mkdir -p "$OUT_DIR"
bk_sweep_runs "$OUT_DIR"

# ---- preflight: who are we, may we do this? --------------------------------------------------------------------------
TMPW="$(bk_mktemp_dir run)"
# IAM database authentication (M8, SR12R-9): BACKUP_IAM_TOKEN_COMMAND prints a fresh short-lived token (e.g. `aws rds generate-db-auth-token ...`; operator-supplied,
# trusted configuration). A token is valid ~15 minutes and only checked when a connection is OPENED, so a fresh private passfile is written before EVERY new
# connection (preflight, snapshot holder, pg_dump, pg_dumpall): a long dump cannot fail at the globals step, and no long-lived password exists.
bk_iam_refresh() {
  [ "${BK_IAM_MODE:-0}" = 1 ] || return 0
  local t
  t="$(bash -c "$BACKUP_IAM_TOKEN_COMMAND" 2>/dev/null)" || bk_die "BACKUP_IAM_TOKEN_COMMAND failed; nothing was dumped"
  [[ "$t" =~ ^[!-~]{8,4096}$ ]] || bk_die "the IAM token command produced no usable single-line token; nothing was dumped"
  t="${t//\\/\\\\}"; t="${t//:/\\:}"
  (umask 077; printf '%s:%s:*:%s:%s\n' "$BK_CANON_HOST" "$BACKUP_PGPORT" "$BACKUP_PGUSER" "$t" >"${TMPW}/iam.pgpass.new") && mv -f "${TMPW}/iam.pgpass.new" "${TMPW}/iam.pgpass"
  export PGPASSFILE="${TMPW}/iam.pgpass"
}
bk_iam_refresh
if ! PRE="$(psql -X -q -At -v ON_ERROR_STOP=1 -F '|' -c "SELECT rolsuper::int, rolbypassrls::int, rolcreaterole::int, rolcreatedb::int, rolreplication::int, current_setting('server_version_num')::int FROM pg_roles WHERE rolname = current_user" 2>"${TMPW}/pre.err")"; then
  bk_die "cannot connect as ${BACKUP_PGUSER} to ${BACKUP_PGHOST}:${BACKUP_PGPORT}/${BACKUP_PGDATABASE}: $(head -2 "${TMPW}/pre.err" | tr '\n' ' ') (typical causes: the time-boxed backup credential EXPIRED - re-mint it with init-backup-role.sql / the secret store job; wrong passfile; role missing; host not allowed)"
fi
IFS='|' read -r R_SUPER R_BYPASS R_CREATEROLE R_CREATEDB R_REPL R_VERNUM <<<"$PRE"
[ "$R_SUPER" = 0 ] || bk_die "refusing to back up as a SUPERUSER (${BACKUP_PGUSER}); use the dedicated NOSUPERUSER BYPASSRLS backup role (ADR 0122)"
[ "$R_BYPASS" = 1 ] || bk_die "role ${BACKUP_PGUSER} does not have BYPASSRLS: under FORCE ROW LEVEL SECURITY a dump by this role is refused or silently INCOMPLETE (BACKUP-ROLE-1). Use the dedicated backup role (deploy/backup/init-backup-role.sql); not weakening RLS to dump"
[ "$R_CREATEROLE" = 0 ] && [ "$R_CREATEDB" = 0 ] && [ "$R_REPL" = 0 ] || bk_die "role ${BACKUP_PGUSER} holds CREATEROLE/CREATEDB/REPLICATION; the backup role must have none of them"
RO="$(bk_sql "SELECT current_setting('default_transaction_read_only')")"
[ "$RO" = on ] || bk_die "backup role session is not read-only by default (default_transaction_read_only=${RO}); re-run init-backup-role.sql"
MISSING="$(bk_sql "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public' WHERE c.relkind IN ('r','p','S') AND NOT has_table_privilege(c.oid, 'SELECT')")"
[ "$MISSING" = 0 ] || bk_die "backup role lacks SELECT on ${MISSING} table(s)/sequence(s) (a new migration table without the default privilege?); re-run init-backup-role.sql. Refusing to produce an incomplete dump"
# F1 (migration 0166): the actor-proof signing key must be OUT OF THIS ROLE'S REACH. Privileges are read by OID (a name lookup in a schema
# without USAGE would itself be refused). Refused: migration 0166 not applied (the key would still be in public, readable by this role), or
# this role holding USAGE/CREATE on actorproof_private or any privilege on a relation in it. The drill-only fault include_actor_proof_keys
# skips this check (it exists to prove the stream assertion below on a deliberately mis-granted role).
APK_PRE="$(bk_sql "SELECT (pg_catalog.to_regnamespace('actorproof_private') IS NOT NULL)::int || '|' || (pg_catalog.to_regclass('public.actor_proof_keys') IS NOT NULL)::int || '|' || ((SELECT count(*) FROM pg_namespace n WHERE n.nspname = 'actorproof_private' AND (has_schema_privilege(n.oid, 'USAGE') OR has_schema_privilege(n.oid, 'CREATE'))) + (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private' WHERE c.relkind IN ('r','p','v','m','S','f') AND (has_table_privilege(c.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege(c.oid, 'SELECT,INSERT,UPDATE,REFERENCES'))))")"
IFS='|' read -r APK_SCHEMA APK_PUBLIC APK_PRIVS <<<"$APK_PRE"
if [ "${BACKUP_TEST_FAULT:-}" != include_actor_proof_keys ] || [ "$BK_TARGET_LOCAL" -ne 1 ]; then
  [ "$APK_SCHEMA" = 1 ] && [ "$APK_PUBLIC" = 0 ] || bk_die "the actor-proof key table is not isolated (schema actorproof_private present: ${APK_SCHEMA}, public.actor_proof_keys present: ${APK_PUBLIC}): migration 0166 is not applied, so this role could read the signing key. Migrate first; nothing was dumped"
  [ "$APK_PRIVS" = 0 ] || bk_die "${BACKUP_PGUSER} holds ${APK_PRIVS} privilege(s) on the schema actorproof_private or its relations: the backup credential can read the actor-proof signing key (owner decision 5). Revoke them (deploy/backup/init-backup-role.sql re-asserts the denial); nothing was dumped"
fi

# ---- encryption: validated BEFORE any dump (H1) -----------------------------------------------------------------------
ENC=none
if [ -n "${BACKUP_ENCRYPT_AGE_RECIPIENT:-}" ] && [ -n "${BACKUP_ENCRYPT_GPG_RECIPIENT:-}" ]; then bk_die "set only one of BACKUP_ENCRYPT_AGE_RECIPIENT / BACKUP_ENCRYPT_GPG_RECIPIENT"; fi
if [ -n "${BACKUP_ENCRYPT_AGE_RECIPIENT:-}" ]; then
  bk_need_tools age
  ENC=age
  [[ "$BACKUP_ENCRYPT_AGE_RECIPIENT" =~ ^age1[a-z0-9]{50,70}$ ]] || bk_die "BACKUP_ENCRYPT_AGE_RECIPIENT is not a well-formed age recipient; refusing to dump (nothing was written)"
  printf probe | age -r "$BACKUP_ENCRYPT_AGE_RECIPIENT" >"${TMPW}/probe.age" 2>"${TMPW}/enc.err" || bk_die "age cannot encrypt to BACKUP_ENCRYPT_AGE_RECIPIENT ($(head -1 "${TMPW}/enc.err")); refusing to dump (nothing was written)"
  [ -s "${TMPW}/probe.age" ] || bk_die "age produced no output for the probe; refusing to dump"
elif [ -n "${BACKUP_ENCRYPT_GPG_RECIPIENT:-}" ]; then
  bk_need_tools gpg
  ENC=gpg
  [[ "$BACKUP_ENCRYPT_GPG_RECIPIENT" =~ ^[0-9A-Fa-f]{40}$ ]] || bk_die "BACKUP_ENCRYPT_GPG_RECIPIENT must be a FULL 40-hex key fingerprint (a short id or a user-id string can be spoofed); refusing to dump"
  gpg --batch --quiet --list-keys "$BACKUP_ENCRYPT_GPG_RECIPIENT" >/dev/null 2>&1 || bk_die "gpg key ${BACKUP_ENCRYPT_GPG_RECIPIENT} is not in the keyring; refusing to dump (nothing was written)"
  printf probe | gpg --batch --yes --quiet --no-tty --trust-model always --recipient "$BACKUP_ENCRYPT_GPG_RECIPIENT" --encrypt >"${TMPW}/probe.gpg" 2>"${TMPW}/enc.err" || bk_die "gpg cannot encrypt to ${BACKUP_ENCRYPT_GPG_RECIPIENT} ($(head -1 "${TMPW}/enc.err")); refusing to dump"
  [ -s "${TMPW}/probe.gpg" ] || bk_die "gpg produced no output for the probe; refusing to dump"
else
  [ "${BACKUP_ALLOW_UNENCRYPTED:-}" = "synthetic-data-only" ] && [ "$BK_TARGET_LOCAL" -eq 1 ] \
    || bk_die "no encryption recipient configured. A real dump contains PII, financial data and signing keys: set BACKUP_ENCRYPT_AGE_RECIPIENT or BACKUP_ENCRYPT_GPG_RECIPIENT (public references only). The override BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only is honoured only for a throwaway drill target on this machine"
fi
rm -f "${TMPW}"/probe.*
enc_stream() { # stdin plaintext -> stdout ciphertext (or plaintext in the synthetic override)
  case "$ENC" in
    age) age -r "$BACKUP_ENCRYPT_AGE_RECIPIENT" ;;
    gpg) gpg --batch --yes --quiet --no-tty --trust-model always --recipient "$BACKUP_ENCRYPT_GPG_RECIPIENT" --encrypt ;;
    none) cat ;;
  esac
}
case "$ENC" in age) EXT=".age" ;; gpg) EXT=".gpg" ;; *) EXT="" ;; esac

# ---- signing key (H3) validated up front -----------------------------------------------------------------------------
[ -n "${BACKUP_SIGNING_KEY_FILE:-}" ] || bk_die "BACKUP_SIGNING_KEY_FILE is required: backup sets must be signed (A12 security review H3)"
bk_check_secret_file "$BACKUP_SIGNING_KEY_FILE" "signing key"
printf probe >"${TMPW}/sigprobe"
openssl pkeyutl -sign -rawin -inkey "$BACKUP_SIGNING_KEY_FILE" -in "${TMPW}/sigprobe" -out "${TMPW}/sigprobe.sig" 2>/dev/null || bk_die "the signing key cannot sign (not an Ed25519 PEM private key, or OpenSSL < 3.0); refusing to dump"
rm -f "${TMPW}"/sigprobe*

MIN_FREE_MB="${BACKUP_MIN_FREE_MB:-512}"
FREE_MB="$(df -Pm "$OUT_DIR" | awk 'NR==2 {print $4}')"
[ "$FREE_MB" -ge "$MIN_FREE_MB" ] || bk_die "insufficient disk space in ${OUT_DIR}: ${FREE_MB} MB free, ${MIN_FREE_MB} MB required (BACKUP_MIN_FREE_MB). A dump needs roughly the compressed database size plus a copy for checksumming; nothing was written"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${OUT_DIR}/${STAMP}-${BACKUP_PGDATABASE}${LABEL:+-$LABEL}"
[ ! -e "$RUN_DIR" ] || { RUN_DIR=""; bk_die "refusing to overwrite an existing run directory"; }
mkdir -m 700 "$RUN_DIR"
printf '%s:%s' "$(hostname)" "$$" >"${RUN_DIR}/.incomplete"

T0="$(now_s)"

# ---- one exported snapshot shared by pg_dump and the baseline queries -------------------------------------------------
mkfifo "${TMPW}/in" "${TMPW}/out"
bk_iam_refresh
psql -X -q -At -v ON_ERROR_STOP=1 <"${TMPW}/in" >"${TMPW}/out" 2>"${TMPW}/holder.err" &
SESSION_PID=$!
exec 7>"${TMPW}/in" 8<"${TMPW}/out"
echo "BEGIN ISOLATION LEVEL REPEATABLE READ, READ ONLY;" >&7
echo "SELECT pg_export_snapshot();" >&7
IFS= read -r SNAP <&8 || bk_die "could not export a snapshot: $(head -2 "${TMPW}/holder.err" | tr '\n' ' ')"
[ -n "$SNAP" ] || bk_die "empty snapshot id"

# baseline evaluated inside the snapshot (plaintext only in the private temp dir; encrypted below; it holds counts and hashes only)
printf '\\o %s\n\\i %s\n\\o\nSELECT %s;\n' "${TMPW}/baseline.txt" "${SCRIPT_DIR}/baseline.sql" "'__BASELINE_DONE__'" >&7
IFS= read -r DONE <&8 || true
[ "$DONE" = "__BASELINE_DONE__" ] || bk_die "baseline queries failed: $(head -3 "${TMPW}/holder.err" | tr '\n' ' ')"
T_BASE="$(now_s)"

# keep the idle snapshot holder alive (idle_in_transaction_session_timeout) and drain its replies; a dead holder is fatal
BK_KEEPALIVE="${BACKUP_KEEPALIVE_SECONDS:-60}"
case "$BK_KEEPALIVE" in ''|*[!0-9]*) bk_die "BACKUP_KEEPALIVE_SECONDS must be an integer" ;; esac
cat <&8 >/dev/null &
DRAIN_PID=$!
(
  while :; do
    for ((i = 0; i < BK_KEEPALIVE; i++)); do sleep 1; done   # 1 s steps: a killed keepalive leaves at most a 1 s orphan holding the pipe
    echo "SELECT 1;" >&7 2>/dev/null || { : >"${TMPW}/holder.dead"; exit 0; }
  done
) &
KEEP_PID=$!

# drill-only fault hooks (honoured only for a throwaway drill target on this machine)
FAULT="${BACKUP_TEST_FAULT:-}"
[ -z "$FAULT" ] || [ "$BK_TARGET_LOCAL" -eq 1 ] || bk_die "BACKUP_TEST_FAULT is honoured only for a throwaway drill target"
if [ "$FAULT" = slow_dump ]; then sleep "${BACKUP_TEST_SLOW_SECONDS:-12}" & SLEEP_PID=$!; wait "$SLEEP_PID" || true; SLEEP_PID=""; fi   # background + wait: a signal is handled at once
[ "$FAULT" != kill_holder ] || { kill "$SESSION_PID" 2>/dev/null; sleep 1; }

# the dump, STREAMED through the encryptor: no plaintext on the backup volume
DUMP_OUT="${RUN_DIR}/db.dump${EXT}"
bk_iam_refresh
# Owner decision 5 / F1: the schema holding the actor-proof HMAC secrets is excluded from the stream (pg_dump neither locks nor reads it). The
# stream is also read by a `pg_restore --list` while it is written, so the exclusion is VERIFIED on the very bytes that are encrypted (not
# assumed from the flag).
EXCL=(--exclude-schema=actorproof_private)
# drill-only negative control: drop the exclusion to prove that the role cannot dump the schema and that the TOC assertion below refuses the backup
[ "$FAULT" != include_actor_proof_keys ] || EXCL=()
(
  set -o pipefail
  pg_dump -Fc --snapshot="$SNAP" "${EXCL[@]}" 2>"${TMPW}/pg_dump.err" | tee >(sha256sum | cut -d' ' -f1 >"${TMPW}/plain.sha") >(pg_restore --list >"${TMPW}/stream.toc" 2>"${TMPW}/toc.err"; echo $? >"${TMPW}/toc.rc") | enc_stream >"${DUMP_OUT}.partial" 2>"${TMPW}/enc.err"
) &
DUMP_PID=$!
while kill -0 "$DUMP_PID" 2>/dev/null; do
  if [ -e "${TMPW}/holder.dead" ] || ! kill -0 "$SESSION_PID" 2>/dev/null; then
    bk_kill_tree "$DUMP_PID"
    bk_die "the snapshot-holder session died during the dump (idle timeout, kill or network loss): the dump cannot be trusted; backup failed, nothing kept"
  fi
  sleep 0.5
done
if ! wait "$DUMP_PID"; then
  DUMP_PID=""
  bk_die "pg_dump or the encryptor failed: $(head -3 "${TMPW}/pg_dump.err" "${TMPW}/enc.err" 2>/dev/null | tr '\n' ' ')"
fi
DUMP_PID=""
[ -s "${DUMP_OUT}.partial" ] || bk_die "the dump is empty"
mv "${DUMP_OUT}.partial" "$DUMP_OUT"
for _ in $(seq 1 50); do [ -s "${TMPW}/plain.sha" ] && break; sleep 0.2; done   # the tee process substitution may finish just after the pipeline
PLAIN_SHA="$(cat "${TMPW}/plain.sha" 2>/dev/null || true)"
[[ "$PLAIN_SHA" =~ ^[0-9a-f]{64}$ ]] || bk_die "could not record the plaintext checksum"
for _ in $(seq 1 100); do [ -s "${TMPW}/toc.rc" ] && break; sleep 0.2; done   # the pg_restore --list process substitution may finish just after the pipeline
[ "$(cat "${TMPW}/toc.rc" 2>/dev/null || echo missing)" = 0 ] || bk_die "could not read the table of contents of the dump stream ($(head -2 "${TMPW}/toc.err" 2>/dev/null | tr '\n' ' ')); the actor-proof exclusion cannot be verified, so no backup is kept"
grep -q ' FUNCTION public actor_proof_require(' "${TMPW}/stream.toc" || bk_die "the dump holds no actor_proof_require verifier (migration 0120 missing, or an unexpected dump); refusing"
if grep -q -E ' TABLE DATA [^ ]+ actor_proof_key[^ ]* ' "${TMPW}/stream.toc"; then
  bk_die "the dump stream carries the DATA of an actor_proof_keys table (the actor-proof signing secrets); logical backups must never contain them (ADR 0122 section 10). No backup is kept"
fi
if grep -q -E '(^| )actorproof_private( |$)' "${TMPW}/stream.toc"; then
  bk_die "the dump stream carries entries of the schema actorproof_private (the actor-proof signing secrets live there); logical backups must exclude it (ADR 0122 section 10, migration 0166). No backup is kept"
fi
APK_DATA=excluded
rm -f "${TMPW}/stream.toc"
T_DUMP="$(now_s)"
kill "$KEEP_PID" 2>/dev/null || true; KEEP_PID=""
echo "COMMIT;" >&7 2>/dev/null || true
exec 7>&-
kill "$SESSION_PID" "$DRAIN_PID" 2>/dev/null || true
wait "$SESSION_PID" "$DRAIN_PID" 2>/dev/null || true
SESSION_PID=""; DRAIN_PID=""
exec 8<&-

# role/grant definitions (no passwords: pg_dumpall without role-password access needs only pg_roles), streamed likewise
bk_iam_refresh
pg_dumpall --globals-only --no-role-passwords --database="${BACKUP_PGDATABASE}" 2>"${TMPW}/globals.err" | enc_stream >"${RUN_DIR}/globals.sql${EXT}" \
  || bk_die "pg_dumpall --globals-only or encryption failed: $(head -3 "${TMPW}/globals.err" | tr '\n' ' ')"
[ -s "${RUN_DIR}/globals.sql${EXT}" ] || bk_die "globals output is empty"
T_GLOB="$(now_s)"

# ---- invariants on the snapshot (read from the plaintext baseline in the private temp dir) -----------------------------
INV_BAD="$(awk -F'|' '$1=="INV" && $3!="0" {n++} END {print n+0}' "${TMPW}/baseline.txt")"
TABLES="$(grep -c '^TABLE|' "${TMPW}/baseline.txt" || true)"
HEAD_LINE="$(grep '^META|migration_head|' "${TMPW}/baseline.txt" | cut -d'|' -f3- || true)"
ANCHOR="$(grep '^META|environment_anchor|' "${TMPW}/baseline.txt" | cut -d'|' -f3- || true)"
[ -n "$TABLES" ] && [ "$TABLES" -gt 0 ] || bk_die "the baseline holds no tables (baseline unusable)"
enc_stream <"${TMPW}/baseline.txt" >"${RUN_DIR}/baseline.txt${EXT}" || bk_die "encrypting the baseline failed"
[ -s "${RUN_DIR}/baseline.txt${EXT}" ] || bk_die "baseline output is empty"
rm -f "${TMPW}/baseline.txt"
T_ENC="$(now_s)"

# ---- what is REALLY on disk: encryption field from magic bytes, and no plaintext artefact may exist (H1) ----------------
REAL_ENC=none
case "$ENC" in
  age) for f in db.dump globals.sql baseline.txt; do [ "$(head -c 21 "${RUN_DIR}/${f}.age")" = "age-encryption.org/v1" ] || bk_die "${f}.age is not an age file"; done; REAL_ENC=age ;;
  gpg) for f in db.dump globals.sql baseline.txt; do gpg --list-packets "${RUN_DIR}/${f}.gpg" 2>/dev/null | grep -q 'pubkey enc packet' || bk_die "${f}.gpg is not a public-key encrypted gpg file"; done; REAL_ENC=gpg ;;
esac
if [ "$REAL_ENC" != none ]; then
  for f in db.dump globals.sql baseline.txt; do [ ! -e "${RUN_DIR}/${f}" ] || bk_die "PLAINTEXT ${f} present in the backup directory after encryption: refusing"; done
fi
if ls "${RUN_DIR}"/*.partial >/dev/null 2>&1; then bk_die "partial artefact left in the backup directory"; fi

# ---- manifest, checksums, signature -----------------------------------------------------------------------------------
DUMP_BYTES="$(stat -c %s "${DUMP_OUT}")"
GIT_COMMIT="$(git -C "${SCRIPT_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)"
cat >"${RUN_DIR}/manifest.json" <<EOF
{
  "format": "igaming-logical-backup/2",
  "created_utc": "${STAMP}",
  "database": "${BACKUP_PGDATABASE}",
  "host_ref": "${BK_CANON_HOST}:${BACKUP_PGPORT}",
  "role": "${BACKUP_PGUSER}",
  "server_version_num": ${R_VERNUM},
  "pg_dump": "$(pg_dump --version | tr -d '\n')",
  "migration_head": "${HEAD_LINE}",
  "environment_anchor": "${ANCHOR:-unknown}",
  "tables_in_baseline": "${TABLES}",
  "encryption": "${REAL_ENC}",
  "actor_proof_keys_data": "${APK_DATA}",
  "signed": "ed25519-sha256sums",
  "plaintext_dump_sha256": "${PLAIN_SHA}",
  "dump_bytes": ${DUMP_BYTES},
  "source_invariant_violations": "${INV_BAD}",
  "repo_commit": "${GIT_COMMIT}",
  "seconds": { "baseline_queries": $(elapsed_s "$T0" "$T_BASE"), "pg_dump_and_encrypt": $(elapsed_s "$T_BASE" "$T_DUMP"), "globals": $(elapsed_s "$T_DUMP" "$T_GLOB"), "encrypt_baseline": $(elapsed_s "$T_GLOB" "$T_ENC"), "total": $(elapsed_s "$T0" "$(now_s)") }
}
EOF
( cd "$RUN_DIR" && sha256sum -- db.dump* globals.sql* baseline.txt* manifest.json > SHA256SUMS )
bk_sign_set "$RUN_DIR"
chmod 600 "${RUN_DIR}"/*
bk_log "backup written: ${RUN_DIR}"
echo "RUN_DIR=${RUN_DIR}"
# the rollback pin: the runner records this out of band (its backup log / ticket); restore.sh requires it as RESTORE_EXPECT_SET_SHA256 off the drill
echo "SET_SHA256=$(sha256sum <"${RUN_DIR}/SHA256SUMS" | cut -d' ' -f1)"
echo "TIMING|backup_total_seconds|$(elapsed_s "$T0" "$(now_s)")"
echo "TIMING|backup_baseline_seconds|$(elapsed_s "$T0" "$T_BASE")"
echo "TIMING|backup_pg_dump_seconds|$(elapsed_s "$T_BASE" "$T_DUMP")"
echo "TIMING|backup_encrypt_seconds|$(elapsed_s "$T_GLOB" "$T_ENC")"
echo "SIZE|dump_bytes|${DUMP_BYTES}"
if [ "$INV_BAD" != 0 ]; then
  echo "backup: SOURCE INVARIANT VIOLATION(S) in the snapshot (see baseline.txt INV lines); backup kept as evidence" >&2
  exit 4
fi
exit 0
