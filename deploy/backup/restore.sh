#!/usr/bin/env bash
# Restore a logical backup (deploy/backup/backup.sh) into a NEW, EMPTY database (ADR 0122). Never restores over an
# existing database. Human-run (or drill-run) operation; never part of any automated pipeline against a shared database.
#
#   BACKUP_PGHOST=... BACKUP_PGPORT=... BACKUP_PGUSER=<restore role> BACKUP_PGDATABASE=postgres \
#   BACKUP_PGPASSFILE=... RESTORE_CONFIRM=igaming_restored \
#   deploy/backup/restore.sh --backup-dir <run dir> --new-database igaming_restored [--apply-globals]
#
# The restore role must be able to CREATE DATABASE and act as the owner role (default `igaming`): the owner role
# itself (RDS master), or a CREATEDB role that is a member of it. It must NOT be igaming_runtime or igaming_backup:
# neither can own objects, and the backup role is SELECT-only by design.
#
# Sequence (each step fail-closed; on any failure the database created by THIS run is dropped):
#   1. checksum verification of every artifact (sha256sum -c) BEFORE any database is touched
#   2. decrypt (age: RESTORE_AGE_IDENTITY_FILE; gpg: the restorer's keyring) into a private 0700 work dir
#   3. pg_restore --list must parse; required roles must exist (or --apply-globals creates the MISSING project roles
#      igaming/igaming_runtime/igaming_backup WITHOUT passwords - credentials are re-provisioned from the secret store)
#   4. refuse if the target database already exists (empty or not) - "restore into non-empty database" is refused
#   5. CREATE DATABASE <new> OWNER igaming TEMPLATE template0 (encoding/collation from the source baseline)
#   6. pg_restore --single-transaction --exit-on-error --no-owner --role=<owner>: ALL or NOTHING. The dump orders
#      pre-data (tables), data (COPY), post-data (indexes, constraints, TRIGGERS, RLS, policies): data therefore loads
#      with NO triggers and NO policies in force, so nothing is disabled, no superuser is needed and no guard is bypassed.
#      (This corrects the earlier assumption in restore-from-backup.md section 3 step 2.)
#   7. post-restore-grants.sql (database-level ACL a dump cannot carry) and ANALYZE
# NOT done here: the database_environment anchor, runtime/backup credentials, `cmd/migrate verify` and the data
# comparison - those are verify.sh and the post-restore checklist (docs/runbooks/backup-restore-dr.md).
#
# Test hook (drill only): RESTORE_TEST_FAULT=kill_midway terminates pg_restore part-way to prove the rollback.
# Exit: 0 ok; 2 refused/usage/preflight; 3 restore failed (target dropped).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"
# shellcheck source=deploy/backup/lib-globals.sh
. "${SCRIPT_DIR}/lib-globals.sh"

BACKUP_DIR=""; NEW_DB=""; APPLY_GLOBALS=0; OWNER_ROLE="igaming"; DO_ANALYZE=1
while [ $# -gt 0 ]; do
  case "$1" in
    --backup-dir) shift; BACKUP_DIR="${1:-}" ;;
    --new-database) shift; NEW_DB="${1:-}" ;;
    --owner-role) shift; OWNER_ROLE="${1:-}" ;;
    --apply-globals) APPLY_GLOBALS=1 ;;
    --no-analyze) DO_ANALYZE=0 ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 0 ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done
[ -d "$BACKUP_DIR" ] || bk_die "--backup-dir must be an existing backup run directory"
[[ "$NEW_DB" =~ ^[a-z][a-z0-9_]{2,40}$ ]] || bk_die "--new-database must match ^[a-z][a-z0-9_]{2,40}\$"
[[ "$OWNER_ROLE" =~ ^[a-z][a-z0-9_]{1,40}$ ]] || bk_die "--owner-role is not a plain identifier"
[ "${RESTORE_CONFIRM:-}" = "$NEW_DB" ] || bk_die "RESTORE_CONFIRM must equal the new database name (${NEW_DB}); this is the explicit human acknowledgement that a restore is intended"

bk_need_tools psql pg_restore sha256sum awk
bk_connect_env
umask 077

T0="$(now_s)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/bk-restore.XXXXXX")"
CREATED_DB=""
cleanup() {
  local rc=$?
  set +e
  if [ -n "$CREATED_DB" ] && [ "$rc" -ne 0 ]; then
    # only the database this very run created, by validated name
    psql -X -q -At -c "DROP DATABASE IF EXISTS \"${CREATED_DB}\" WITH (FORCE)" >/dev/null 2>&1
    echo "restore: FAILED (exit ${rc}); database ${CREATED_DB} created by this run was dropped" >&2
  fi
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT

# ---- 1. integrity first --------------------------------------------------------------------------------------
[ -f "${BACKUP_DIR}/SHA256SUMS" ] || bk_die "no SHA256SUMS in ${BACKUP_DIR}: not a backup run directory"
if ! ( cd "$BACKUP_DIR" && sha256sum --check --strict --quiet SHA256SUMS ) >"${WORK}/sum.out" 2>&1; then
  bk_die "CHECKSUM MISMATCH or missing file in ${BACKUP_DIR} - the backup is corrupted or tampered with; nothing was restored. $(head -3 "${WORK}/sum.out" | tr '\n' ' ')"
fi
for f in db.dump globals.sql baseline.txt; do
  ls "${BACKUP_DIR}/${f}"* >/dev/null 2>&1 || bk_die "artifact ${f} missing from ${BACKUP_DIR}"
done

# ---- 2. decrypt into the private work dir ---------------------------------------------------------------------
materialise() { # name -> path of the plaintext copy
  local f="$1"
  if [ -f "${BACKUP_DIR}/${f}" ]; then printf '%s' "${BACKUP_DIR}/${f}"; return; fi
  if [ -f "${BACKUP_DIR}/${f}.age" ]; then
    bk_need_tools age
    [ -n "${RESTORE_AGE_IDENTITY_FILE:-}" ] && [ -f "${RESTORE_AGE_IDENTITY_FILE}" ] || bk_die "backup is age-encrypted: RESTORE_AGE_IDENTITY_FILE (path to the private identity, from the secret store) is required"
    age -d -i "$RESTORE_AGE_IDENTITY_FILE" -o "${WORK}/${f}" "${BACKUP_DIR}/${f}.age" || bk_die "age decryption of ${f} failed (wrong identity?)"
    printf '%s' "${WORK}/${f}"; return
  fi
  if [ -f "${BACKUP_DIR}/${f}.gpg" ]; then
    bk_need_tools gpg
    gpg --batch --yes --quiet --output "${WORK}/${f}" --decrypt "${BACKUP_DIR}/${f}.gpg" || bk_die "gpg decryption of ${f} failed (key not available?)"
    printf '%s' "${WORK}/${f}"; return
  fi
  bk_die "artifact ${f} not found"
}
DUMP="$(materialise db.dump)"; GLOBALS="$(materialise globals.sql)"; BASELINE="$(materialise baseline.txt)"

# ---- 3. the archive must parse; required roles ----------------------------------------------------------------
pg_restore --list "$DUMP" >"${WORK}/toc.txt" 2>"${WORK}/toc.err" || bk_die "pg_restore cannot read the dump (truncated or not a custom-format archive): $(head -2 "${WORK}/toc.err" | tr '\n' ' ')"
ME="$(bk_sql "SELECT current_user")"
case "$ME" in
  igaming_runtime|igaming_backup) bk_die "restoring as ${ME} is refused: the runtime role owns nothing and must never create objects; the backup role is SELECT-only. Use the restore role (the database owner role, or a CREATEDB role that is a member of ${OWNER_ROLE})" ;;
esac
IFS='|' read -r R_SUPER R_CREATEDB R_CREATEROLE R_MEMBER <<<"$(psql -X -q -At -F '|' -v ON_ERROR_STOP=1 -c "SELECT rolsuper::int, rolcreatedb::int, rolcreaterole::int, (current_user = '${OWNER_ROLE}' OR CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${OWNER_ROLE}') THEN pg_has_role(current_user, '${OWNER_ROLE}', 'MEMBER') ELSE false END)::int FROM pg_roles WHERE rolname = current_user")"
[ "$R_SUPER" = 1 ] || [ "$R_CREATEDB" = 1 ] || bk_die "restore role ${ME} lacks CREATEDB; cannot create the target database"
[ "$R_SUPER" = 1 ] || [ "$R_MEMBER" = 1 ] || bk_die "restore role ${ME} is neither ${OWNER_ROLE} nor a member of it; the restored objects must be owned by ${OWNER_ROLE}"

PROJECT_ROLES="igaming igaming_runtime igaming_backup"
MISSING_ROLES=""
for r in $PROJECT_ROLES; do
  [ "$(bk_sql "SELECT count(*) FROM pg_roles WHERE rolname = '$r'")" = 1 ] || MISSING_ROLES="${MISSING_ROLES} $r"
done
if [ -n "$MISSING_ROLES" ]; then
  [ "$APPLY_GLOBALS" -eq 1 ] || bk_die "roles missing on the target cluster:${MISSING_ROLES}. The dump's GRANTs reference them. Re-run with --apply-globals (creates ONLY these project roles, WITHOUT passwords, from globals.sql) or create them from the environment's role-provisioning script first"
  for r in $MISSING_ROLES; do
    awk -v r="$r" '$0 == "CREATE ROLE " r ";" || index($0, "ALTER ROLE " r " ") == 1' "$GLOBALS" >"${WORK}/globals-${r}.sql"
    [ -s "${WORK}/globals-${r}.sql" ] || bk_die "globals.sql has no definition for role ${r}"
    # the manifest is unsigned: never replay a role statement outside the expected shapes (e.g. ALTER ROLE ... SUPERUSER)
    bk_validate_role_stmts "$r" "${WORK}/globals-${r}.sql" 2>"${WORK}/g.val" \
      || bk_die "globals.sql contains a tampered or unexpected role statement; NO role or database was created. $(head -2 "${WORK}/g.val" | tr '\n' ' ')"
    psql -X -q -At -v ON_ERROR_STOP=1 -f "${WORK}/globals-${r}.sql" >/dev/null 2>"${WORK}/g.err" \
      || bk_die "creating role ${r} failed (BYPASSRLS needs a superuser on stock PostgreSQL): $(head -2 "${WORK}/g.err" | tr '\n' ' ')"
    bk_log "created role ${r} WITHOUT a password (re-provision its credential from the secret store)"
  done
fi

# ---- 4. never over an existing database -----------------------------------------------------------------------
if [ "$(bk_sql "SELECT count(*) FROM pg_database WHERE datname = '${NEW_DB}'")" != 0 ]; then
  bk_die "database ${NEW_DB} already exists: refusing to restore into an existing database (empty or not). Choose a new name; restore never overwrites"
fi
MIN_FREE_MB="${RESTORE_MIN_FREE_MB:-0}"
if [ "$MIN_FREE_MB" -gt 0 ]; then
  FREE_MB="$(df -Pm "$WORK" | awk 'NR==2 {print $4}')"
  [ "$FREE_MB" -ge "$MIN_FREE_MB" ] || bk_die "insufficient disk space in the work directory: ${FREE_MB} MB free, ${MIN_FREE_MB} MB required (RESTORE_MIN_FREE_MB)"
fi

# ---- 5. create ---------------------------------------------------------------------------------------------------
SET_LINE="$(grep '^META|dbsettings|' "$BASELINE" | cut -d'|' -f3- || true)"
[ -n "$SET_LINE" ] || bk_die "baseline.txt has no META|dbsettings line (backup produced by an older/foreign tool?); refusing to guess encoding/collation"
ENC="$(sed -n 's/.*encoding=\([^ ]*\).*/\1/p' <<<"$SET_LINE")"; COL="$(sed -n 's/.*collate=\([^ ]*\).*/\1/p' <<<"$SET_LINE")"; CTY="$(sed -n 's/.*ctype=\([^ ]*\).*/\1/p' <<<"$SET_LINE")"
[[ "${ENC:-UTF8}" =~ ^[A-Z0-9_]+$ && "${COL:-C}" =~ ^[A-Za-z0-9_.@-]+$ && "${CTY:-C}" =~ ^[A-Za-z0-9_.@-]+$ ]] || bk_die "unsafe database settings in baseline: ${SET_LINE}"
T_CREATE="$(now_s)"
psql -X -q -At -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"${NEW_DB}\" OWNER \"${OWNER_ROLE}\" TEMPLATE template0 ENCODING '${ENC:-UTF8}' LC_COLLATE '${COL:-C}' LC_CTYPE '${CTY:-C}'" >/dev/null 2>"${WORK}/c.err" \
  || bk_die "CREATE DATABASE failed: $(head -2 "${WORK}/c.err" | tr '\n' ' ')"
CREATED_DB="$NEW_DB"

# ---- 6. all-or-nothing restore --------------------------------------------------------------------------------
T_RESTORE0="$(now_s)"
if [ "${RESTORE_TEST_FAULT:-}" = "kill_midway" ]; then
  # DRILL-ONLY fault: terminate the pg_restore client once the new database has grown (an operator Ctrl-C or a lost
  # connection part-way). The single transaction must roll back and the half-created database must be dropped.
  PGDATABASE="$NEW_DB" pg_restore --single-transaction --exit-on-error --no-owner --role="$OWNER_ROLE" \
    --dbname="$NEW_DB" "$DUMP" >"${WORK}/restore.out" 2>"${WORK}/restore.err" &
  RPID=$!
  SIZE0="$(bk_sql "SELECT pg_database_size('${NEW_DB}')")"
  for _ in $(seq 1 600); do
    kill -0 "$RPID" 2>/dev/null || break
    SIZE_NOW="$(bk_sql "SELECT pg_database_size('${NEW_DB}')")"
    [ $((SIZE_NOW - SIZE0)) -gt "${RESTORE_FAULT_AFTER_BYTES:-3000000}" ] && break
    sleep 0.1
  done
  kill "$RPID" 2>/dev/null || true
  wait "$RPID" 2>/dev/null && RC_RESTORE=0 || RC_RESTORE=$?
  [ "$RC_RESTORE" -ne 0 ] || { bk_log "pg_restore finished before the fault could be injected"; exit 3; }
  bk_log "pg_restore failed: terminated part-way by the injected fault (exit ${RC_RESTORE})"; exit 3
fi
PGDATABASE="$NEW_DB" pg_restore --single-transaction --exit-on-error --no-owner --role="$OWNER_ROLE" \
  --dbname="$NEW_DB" "$DUMP" >"${WORK}/restore.out" 2>"${WORK}/restore.err" \
  || { cp "${WORK}/restore.err" "${WORK}/restore.err.keep"; bk_log "pg_restore failed: $(head -4 "${WORK}/restore.err" | tr '\n' ' ')"; exit 3; }
T_RESTORE1="$(now_s)"

# ---- 7. database-level privileges, statistics ------------------------------------------------------------------
PGDATABASE="$NEW_DB" PGOPTIONS="-c role=${OWNER_ROLE}" psql -X -q -At -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/post-restore-grants.sql" >/dev/null \
  || { bk_log "post-restore-grants.sql failed"; exit 3; }
T_GRANTS="$(now_s)"
if [ "$DO_ANALYZE" -eq 1 ]; then
  PGDATABASE="$NEW_DB" PGOPTIONS="-c role=${OWNER_ROLE}" psql -X -q -At -v ON_ERROR_STOP=1 -c "SET client_min_messages = error" -c "ANALYZE" >/dev/null || { bk_log "ANALYZE failed"; exit 3; }
fi
T_END="$(now_s)"
CREATED_DB=""   # success: keep it

bk_log "restored into ${NEW_DB} (owner ${OWNER_ROLE})"
echo "TIMING|restore_total_seconds|$(elapsed_s "$T0" "$T_END")"
echo "TIMING|restore_integrity_and_decrypt_seconds|$(elapsed_s "$T0" "$T_CREATE")"
echo "TIMING|restore_pg_restore_seconds|$(elapsed_s "$T_RESTORE0" "$T_RESTORE1")"
echo "TIMING|restore_grants_seconds|$(elapsed_s "$T_RESTORE1" "$T_GRANTS")"
echo "TIMING|restore_analyze_seconds|$(elapsed_s "$T_GRANTS" "$T_END")"
cat <<EOF
restore: NEXT (not done by this script): re-provision credentials from the secret store (igaming_runtime, igaming_backup,
restore: owner), run the environment role-provisioning script, record the database_environment anchor if this restore
restore: becomes production (cmd/platform-api refuses to start otherwise), then run deploy/backup/verify.sh, then
restore: reconcile before opening traffic. See docs/runbooks/backup-restore-dr.md "Post-restore checklist".
EOF
