#!/usr/bin/env bash
# Restore a logical backup (deploy/backup/backup.sh) into a NEW, EMPTY database (ADR 0122). Never restores over an existing populated
# database. Human-run (or drill-run) operation; never part of any automated pipeline against a shared database.
#
#   RESTORE_CONFIRM=<new db> RESTORE_EXPECT_SOURCE_ENVIRONMENT=<production|development|test|unrecorded> \
#   RESTORE_VERIFY_PUBKEY_FILE=<pem> RESTORE_VERIFY_PUBKEY_SHA256=<pinned fingerprint> \
#   BACKUP_PGHOST=... BACKUP_PGPORT=... BACKUP_PGUSER=<role> BACKUP_PGDATABASE=<maintenance db> BACKUP_PGPASSFILE=... \
#   deploy/backup/restore.sh --backup-dir <run dir> --new-database igaming_restored [--phase all|provision|load] [--apply-globals]
#
# TWO PRIVILEGE LEVELS, NEVER ONE (A12 security review H3). A dump is executable SQL; whoever runs `pg_restore` gives the archive that
# session's rights. Therefore:
#   * phase `provision` (needs CREATEDB, and a superuser only to create the three project roles with --apply-globals): creates missing
#     roles WITHOUT passwords from allow-listed statements and an EMPTY database owned by the owner role. Nothing from the archive runs.
#   * phase `load` (runs the archive): the connecting role must be a NON-superuser, not a member of rds_superuser, and the owner role
#     itself (or a CREATEDB role that is a member of it). A planted `COPY ... TO PROGRAM`, `ALTER ROLE ... SUPERUSER`, `RESET ROLE` or
#     function body therefore has no privilege to abuse and fails the single-transaction restore harmlessly.
#   * phase `all` (default): both with ONE identity, which must then satisfy the `load` rule (the same-cluster / non-superuser case).
#   A privileged data-load role is refused; the drill-only override RESTORE_ALLOW_PRIVILEGED_DATA_ROLE=drill-only works only for a
#   throwaway drill target on this machine (it exists to prove the attack in the negative-control test).
#
# Sequence (each step fail-closed; on any failure the database created by THIS run is dropped):
#   0. arguments, owner role allow-list (`igaming`; RESTORE_OWNER_ROLE_OVERRIDE=<name> for an explicitly different staging owner; never
#      postgres/igaming_backup/igaming_runtime, never a superuser or BYPASSRLS role)
#   1. SIGNATURE of SHA256SUMS against the PINNED public key (before checksumming, decrypting or any database contact), then the
#      checksums of every artefact, then the required artefact names must all be listed in the signed manifest
#   2. decrypt (age: RESTORE_AGE_IDENTITY_FILE; gpg: the restorer's keyring) into a private 0700 directory under BACKUP_TMPDIR
#   3. pg_restore --list must parse; the source environment anchor must equal RESTORE_EXPECT_SOURCE_ENVIRONMENT, and a non-production
#      source is refused for any target that is not a throwaway drill instance
#   4. refuse an existing populated database; CREATE DATABASE ... TEMPLATE template0 (provision) / require it empty (load)
#   5. pg_restore --single-transaction --exit-on-error --no-owner --role=<owner>: ALL or NOTHING; data loads before any trigger or policy
#      exists (the archive is pre-data/data/post-data). The data of `database_environment` is NOT restored (M4): the anchor of a restored
#      database is BLANK, so a production start refuses until the owner records the right value deliberately
#   6. post-restore-grants.sql (database ACL, schema owner) and ANALYZE
# NOT done here: credentials, the anchor, `cmd/migrate verify`, the data comparison (verify.sh), reconciliation.
#
# Test hook (drill only): RESTORE_TEST_FAULT=kill_midway terminates pg_restore part-way to prove the rollback.
# Exit: 0 ok; 2 refused/usage/preflight; 3 restore failed (target dropped).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"
# shellcheck source=deploy/backup/lib-sign.sh
. "${SCRIPT_DIR}/lib-sign.sh"
# shellcheck source=deploy/backup/lib-globals.sh
. "${SCRIPT_DIR}/lib-globals.sh"

BACKUP_DIR=""; NEW_DB=""; APPLY_GLOBALS=0; OWNER_ROLE="igaming"; DO_ANALYZE=1; PHASE=all
while [ $# -gt 0 ]; do
  case "$1" in
    --backup-dir) shift; BACKUP_DIR="${1:-}" ;;
    --new-database) shift; NEW_DB="${1:-}" ;;
    --owner-role) shift; OWNER_ROLE="${1:-}" ;;
    --phase) shift; PHASE="${1:-}" ;;
    --apply-globals) APPLY_GLOBALS=1 ;;
    --no-analyze) DO_ANALYZE=0 ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 0 ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done
[ -d "$BACKUP_DIR" ] || bk_die "--backup-dir must be an existing backup run directory"
[[ "$NEW_DB" =~ ^[a-z][a-z0-9_]{2,40}$ ]] || bk_die "--new-database must match ^[a-z][a-z0-9_]{2,40}\$"
case "$PHASE" in all|provision|load) ;; *) bk_die "--phase must be all, provision or load" ;; esac
[ "${RESTORE_CONFIRM:-}" = "$NEW_DB" ] || bk_die "RESTORE_CONFIRM must equal the new database name (${NEW_DB}); this is the explicit human acknowledgement that a restore is intended"
# owner role allow-list (M3)
bk_validate_ident "--owner-role" "$OWNER_ROLE"
case "$OWNER_ROLE" in postgres|igaming_backup|igaming_runtime|rdsadmin|rds_superuser) bk_die "--owner-role ${OWNER_ROLE} is refused: it must not be a superuser, the backup role or the runtime role" ;; esac
if [ "$OWNER_ROLE" != igaming ] && [ "${RESTORE_OWNER_ROLE_OVERRIDE:-}" != "$OWNER_ROLE" ]; then
  bk_die "--owner-role must be igaming (the migration owner); a different staging name needs RESTORE_OWNER_ROLE_OVERRIDE=${OWNER_ROLE} (explicit, logged)"
fi

bk_need_tools psql pg_restore sha256sum awk openssl mktemp
bk_connect_env
umask 077
bk_sweep_stale

T0="$(now_s)"
WORK="$(bk_mktemp_dir restore)"
CREATED_DB=""
RPID=""
trap '' PIPE
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
cleanup() {
  local rc=$?
  set +e
  [ -n "$RPID" ] && kill "$RPID" 2>/dev/null
  if [ -n "$CREATED_DB" ] && [ "$rc" -ne 0 ]; then
    # only the database this very run created, by validated name
    psql -X -q -At -c "DROP DATABASE IF EXISTS \"${CREATED_DB}\" WITH (FORCE)" >/dev/null 2>&1
    echo "restore: FAILED (exit ${rc}); database ${CREATED_DB} created by this run was dropped" >&2
  fi
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT

# ---- 1. authenticity first, then integrity ---------------------------------------------------------------------
bk_verify_set_signature "$BACKUP_DIR"
if ! ( cd "$BACKUP_DIR" && sha256sum --check --strict --quiet SHA256SUMS ) >"${WORK}/sum.out" 2>&1; then
  bk_die "CHECKSUM MISMATCH or missing file in ${BACKUP_DIR} - the backup is corrupted or tampered with; nothing was restored. $(head -3 "${WORK}/sum.out" | tr '\n' ' ')"
fi
for f in db.dump globals.sql baseline.txt manifest.json; do
  # every artefact the restore reads must be covered by the SIGNED manifest (an unlisted replacement file would not be checked)
  n="$(awk -v f="$f" '{ n = $2; sub(/^\*/, "", n); sub(/\.(age|gpg)$/, "", n); if (n == f) c++ } END { print c + 0 }' "${BACKUP_DIR}/SHA256SUMS")"
  [ "$n" = 1 ] || bk_die "the signed SHA256SUMS does not list exactly one ${f} artefact (${n}); refusing"
done

# ---- 2. decrypt into the private work dir ---------------------------------------------------------------------------
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

# ---- 3. the archive must parse; source environment (M4) -------------------------------------------------------------
pg_restore --list "$DUMP" >"${WORK}/toc.txt" 2>"${WORK}/toc.err" || bk_die "pg_restore cannot read the dump (truncated or not a custom-format archive): $(head -2 "${WORK}/toc.err" | tr '\n' ' ')"
SRC_ENV="$(grep '^META|environment_anchor|' "$BASELINE" | cut -d'|' -f3- || true)"
[ -n "$SRC_ENV" ] || bk_die "baseline.txt carries no source environment anchor (META|environment_anchor); refusing to guess what is being restored"
[ "${RESTORE_EXPECT_SOURCE_ENVIRONMENT:-}" = "$SRC_ENV" ] \
  || bk_die "RESTORE_EXPECT_SOURCE_ENVIRONMENT must equal the backup's source environment anchor ('${SRC_ENV}'): state what you believe you are restoring; a mismatch is refused"
if [ "$SRC_ENV" != production ] && [ "$BK_TARGET_LOCAL" -ne 1 ]; then
  bk_die "the backup's source environment is '${SRC_ENV}', not production: restoring a development/test/unrecorded source into a non-drill target is refused (synthetic accounts and known test passwords would enter that environment)"
fi

ME="$(bk_sql "SELECT current_user")"
case "$ME" in
  igaming_runtime|igaming_backup) bk_die "restoring as ${ME} is refused: the runtime role owns nothing and must never create objects; the backup role is SELECT-only. Use the restore role (the owner role, or a CREATEDB role that is a member of ${OWNER_ROLE})" ;;
esac
IFS='|' read -r R_SUPER R_CREATEDB R_PRIV R_MEMBER <<<"$(psql -X -q -At -F '|' -v ON_ERROR_STOP=1 -c "SELECT rolsuper::int, rolcreatedb::int, (EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'rds_superuser') AND pg_has_role(current_user, 'rds_superuser', 'MEMBER'))::int, (current_user = '${OWNER_ROLE}' OR CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${OWNER_ROLE}') THEN pg_has_role(current_user, '${OWNER_ROLE}', 'MEMBER') ELSE false END)::int FROM pg_roles WHERE rolname = current_user")"
if [ "$PHASE" != provision ]; then
  if [ "$R_SUPER" = 1 ] || [ "$R_PRIV" = 1 ]; then
    [ "${RESTORE_ALLOW_PRIVILEGED_DATA_ROLE:-}" = "drill-only" ] && [ "$BK_TARGET_LOCAL" -eq 1 ] \
      || bk_die "refusing to run the archive as ${ME}: a superuser / rds_superuser member would give a (possibly planted) archive entry its privileges (COPY ... TO PROGRAM, ALTER ROLE, RESET ROLE). Create the roles and the empty database with --phase provision, then run --phase load as the non-superuser owner role ${OWNER_ROLE}"
  fi
  [ "$R_SUPER" = 1 ] || [ "$R_MEMBER" = 1 ] || bk_die "restore role ${ME} is neither ${OWNER_ROLE} nor a member of it; the restored objects must be owned by ${OWNER_ROLE}"
fi
if [ "$PHASE" != load ]; then
  [ "$R_SUPER" = 1 ] || [ "$R_CREATEDB" = 1 ] || bk_die "restore role ${ME} lacks CREATEDB; cannot create the target database"
  if [ "$PHASE" = all ]; then
    [ "$R_SUPER" = 1 ] || [ "$R_MEMBER" = 1 ] || bk_die "restore role ${ME} is neither ${OWNER_ROLE} nor a member of it"
  fi
fi

# roles (provision / all)
PROJECT_ROLES="igaming igaming_runtime igaming_backup"
if [ "$PHASE" != load ]; then
  MISSING_ROLES=""
  for r in $PROJECT_ROLES; do
    [ "$(bk_sql "SELECT count(*) FROM pg_roles WHERE rolname = '$r'")" = 1 ] || MISSING_ROLES="${MISSING_ROLES} $r"
  done
  if [ -n "$MISSING_ROLES" ]; then
    [ "$APPLY_GLOBALS" -eq 1 ] || bk_die "roles missing on the target cluster:${MISSING_ROLES}. The dump's GRANTs reference them. Re-run with --apply-globals (creates ONLY these project roles, WITHOUT passwords, from allow-listed statements) or create them from the environment's role-provisioning script first"
    for r in $MISSING_ROLES; do
      # only CREATE ROLE and the attribute statement are considered; ALTER ROLE ... SET is never replayed (the repository scripts re-apply session defaults)
      awk -v r="$r" '$0 == "CREATE ROLE " r ";" || (index($0, "ALTER ROLE " r " WITH ") == 1)' "$GLOBALS" >"${WORK}/globals-${r}.sql"
      [ -s "${WORK}/globals-${r}.sql" ] || bk_die "globals.sql has no definition for role ${r}"
      # the manifest is signed, but never replay a role statement outside the expected shapes (e.g. ALTER ROLE ... SUPERUSER)
      bk_validate_role_stmts "$r" "${WORK}/globals-${r}.sql" 2>"${WORK}/g.val" \
        || bk_die "globals.sql contains a tampered or unexpected role statement; NO role or database was created. $(head -2 "${WORK}/g.val" | tr '\n' ' ')"
    done
    for r in $MISSING_ROLES; do
      psql -X -q -At -v ON_ERROR_STOP=1 -f "${WORK}/globals-${r}.sql" >/dev/null 2>"${WORK}/g.err" \
        || bk_die "creating role ${r} failed (BYPASSRLS needs a superuser on stock PostgreSQL): $(head -2 "${WORK}/g.err" | tr '\n' ' ')"
      bk_log "created role ${r} WITHOUT a password (re-provision its credential from the secret store)"
    done
  fi
fi
# the owner role must be unprivileged (M3)
if [ "$(bk_sql "SELECT count(*) FROM pg_roles WHERE rolname = '${OWNER_ROLE}'")" = 1 ]; then
  [ "$(bk_sql "SELECT (NOT rolsuper AND NOT rolbypassrls AND NOT rolcreaterole)::int FROM pg_roles WHERE rolname = '${OWNER_ROLE}'")" = 1 ] \
    || bk_die "owner role ${OWNER_ROLE} is a superuser / BYPASSRLS / CREATEROLE role: refusing to make it the owner of the restored objects"
elif [ "$PHASE" = load ]; then
  bk_die "owner role ${OWNER_ROLE} does not exist on the target; run --phase provision first"
fi

# ---- 4. database state ---------------------------------------------------------------------------------------------
EXISTS_DB="$(bk_sql "SELECT count(*) FROM pg_database WHERE datname = '${NEW_DB}'")"
if [ "$PHASE" = load ]; then
  [ "$EXISTS_DB" = 1 ] || bk_die "database ${NEW_DB} does not exist; run --phase provision first"
  [ "$(bk_sql "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '${NEW_DB}'")" = "$OWNER_ROLE" ] || bk_die "database ${NEW_DB} is not owned by ${OWNER_ROLE}; refusing to load into it"
  NTAB="$(PGDATABASE="$NEW_DB" bk_sql "SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast') AND c.relkind IN ('r','p','v','m','S','f')) + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema'))")" \
    || bk_die "cannot inspect database ${NEW_DB}"
  [ "$NTAB" = 0 ] || bk_die "database ${NEW_DB} is not empty (${NTAB} objects): refusing to restore into an existing populated database. Choose a new name; restore never overwrites"
else
  [ "$EXISTS_DB" = 0 ] || bk_die "database ${NEW_DB} already exists: refusing to restore into an existing database. Choose a new name; restore never overwrites"
fi
MIN_FREE_MB="${RESTORE_MIN_FREE_MB:-0}"
if [ "$MIN_FREE_MB" -gt 0 ]; then
  FREE_MB="$(df -Pm "$WORK" | awk 'NR==2 {print $4}')"
  [ "$FREE_MB" -ge "$MIN_FREE_MB" ] || bk_die "insufficient disk space in the work directory: ${FREE_MB} MB free, ${MIN_FREE_MB} MB required (RESTORE_MIN_FREE_MB)"
fi

# ---- 5. create (provision / all) ----------------------------------------------------------------------------------
T_CREATE="$(now_s)"
if [ "$PHASE" != load ]; then
  SET_LINE="$(grep '^META|dbsettings|' "$BASELINE" | cut -d'|' -f3- || true)"
  [ -n "$SET_LINE" ] || bk_die "baseline.txt has no META|dbsettings line (backup produced by an older/foreign tool?); refusing to guess encoding/collation"
  ENC="$(sed -n 's/.*encoding=\([^ ]*\).*/\1/p' <<<"$SET_LINE")"; COL="$(sed -n 's/.*collate=\([^ ]*\).*/\1/p' <<<"$SET_LINE")"; CTY="$(sed -n 's/.*ctype=\([^ ]*\).*/\1/p' <<<"$SET_LINE")"
  [[ "${ENC:-UTF8}" =~ ^[A-Z0-9_]+$ && "${COL:-C}" =~ ^[A-Za-z0-9_.@-]+$ && "${CTY:-C}" =~ ^[A-Za-z0-9_.@-]+$ ]] || bk_die "unsafe database settings in baseline: ${SET_LINE}"
  psql -X -q -At -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"${NEW_DB}\" OWNER \"${OWNER_ROLE}\" TEMPLATE template0 ENCODING '${ENC:-UTF8}' LC_COLLATE '${COL:-C}' LC_CTYPE '${CTY:-C}'" >/dev/null 2>"${WORK}/c.err" \
    || bk_die "CREATE DATABASE failed: $(head -2 "${WORK}/c.err" | tr '\n' ' ')"
  CREATED_DB="$NEW_DB"
  if [ "$PHASE" = provision ]; then
    CREATED_DB=""
    bk_log "provisioned empty database ${NEW_DB} (owner ${OWNER_ROLE}); roles: $(bk_sql "SELECT string_agg(rolname, ',' ORDER BY rolname) FROM pg_roles WHERE rolname LIKE 'igaming%'")"
    echo "TIMING|restore_provision_seconds|$(elapsed_s "$T0" "$(now_s)")"
    echo "restore: NEXT: set the roles' passwords from the secret store, then run --phase load as ${OWNER_ROLE}."
    exit 0
  fi
fi
[ "$PHASE" = all ] || CREATED_DB="$NEW_DB"   # a failed load drops the (empty) database again if the owner may

# ---- 6. all-or-nothing restore --------------------------------------------------------------------------------------
# the environment anchor DATA is not restored (M4): select the TOC entries without it
grep -v ' TABLE DATA public database_environment ' "${WORK}/toc.txt" >"${WORK}/toc.filtered"
SKIPPED=$(( $(grep -c . "${WORK}/toc.txt") - $(grep -c . "${WORK}/toc.filtered") ))
T_RESTORE0="$(now_s)"
if [ "${RESTORE_TEST_FAULT:-}" = "kill_midway" ]; then
  [ "$BK_TARGET_LOCAL" -eq 1 ] || bk_die "RESTORE_TEST_FAULT is honoured only for a throwaway drill target"
  # DRILL-ONLY fault: terminate the pg_restore client once the new database has grown (an operator Ctrl-C or a lost
  # connection part-way). The single transaction must roll back and the half-created database must be dropped.
  PGDATABASE="$NEW_DB" pg_restore --single-transaction --exit-on-error --no-owner --role="$OWNER_ROLE" -L "${WORK}/toc.filtered" \
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
  RPID=""
  [ "$RC_RESTORE" -ne 0 ] || { bk_log "pg_restore finished before the fault could be injected"; exit 3; }
  bk_log "pg_restore failed: terminated part-way by the injected fault (exit ${RC_RESTORE})"; exit 3
fi
PGDATABASE="$NEW_DB" pg_restore --single-transaction --exit-on-error --no-owner --role="$OWNER_ROLE" -L "${WORK}/toc.filtered" \
  --dbname="$NEW_DB" "$DUMP" >"${WORK}/restore.out" 2>"${WORK}/restore.err" \
  || { bk_log "pg_restore failed: $(head -4 "${WORK}/restore.err" | tr '\n' ' ')"; exit 3; }
T_RESTORE1="$(now_s)"

# ---- 7. database-level privileges, statistics ------------------------------------------------------------------------
PGDATABASE="$NEW_DB" PGOPTIONS="-c role=${OWNER_ROLE}" psql -X -q -At -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/post-restore-grants.sql" >/dev/null \
  || { bk_log "post-restore-grants.sql failed"; exit 3; }
T_GRANTS="$(now_s)"
if [ "$DO_ANALYZE" -eq 1 ]; then
  PGDATABASE="$NEW_DB" PGOPTIONS="-c role=${OWNER_ROLE}" psql -X -q -At -v ON_ERROR_STOP=1 -c "SET client_min_messages = error" -c "ANALYZE" >/dev/null || { bk_log "ANALYZE failed"; exit 3; }
fi
T_END="$(now_s)"
CREATED_DB=""   # success: keep it

bk_log "restored into ${NEW_DB} (owner ${OWNER_ROLE}); source environment '${SRC_ENV}'; the environment anchor was NOT restored (${SKIPPED} TOC entry skipped)"
echo "TIMING|restore_total_seconds|$(elapsed_s "$T0" "$T_END")"
echo "TIMING|restore_integrity_and_decrypt_seconds|$(elapsed_s "$T0" "$T_CREATE")"
echo "TIMING|restore_pg_restore_seconds|$(elapsed_s "$T_RESTORE0" "$T_RESTORE1")"
echo "TIMING|restore_grants_seconds|$(elapsed_s "$T_RESTORE1" "$T_GRANTS")"
echo "TIMING|restore_analyze_seconds|$(elapsed_s "$T_GRANTS" "$T_END")"
cat <<EOF
restore: NEXT (not done by this script): re-provision credentials from the secret store (igaming_runtime, igaming_backup, owner), run the
restore: environment role-provisioning script, then run deploy/backup/verify.sh, then reconcile before opening traffic. The
restore: database_environment anchor is BLANK: a production platform-api refuses to start until the owner records it - record 'production'
restore: ONLY after confirming that the source environment above was production. See docs/runbooks/backup-restore-dr.md "Post-restore checklist".
EOF
