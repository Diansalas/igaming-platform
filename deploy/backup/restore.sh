#!/usr/bin/env bash
# Restore a logical backup (deploy/backup/backup.sh) into a NEW, EMPTY database (ADR 0122). Never restores over an existing populated
# database. Human-run (or drill-run) operation; never part of any automated pipeline against a shared database.
#
#   RESTORE_CONFIRM=<new db> RESTORE_EXPECT_SOURCE_ENVIRONMENT=<anchor of the set> RESTORE_TARGET_ENVIRONMENT=<must be the same> \
#   RESTORE_EXPECT_SET_SHA256=<sha256 of SHA256SUMS from the runner's out-of-band log; required off the drill> \
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
#      The rows of `actor_proof_keys` are not restored either (owner decision 5, ADR 0122 section 10): a restored database has NO actor-proof key
#      and fails closed on every governed write until deploy/backup/rekey-actor-proof.sh provisions a NEW one
#   6. post-restore-grants.sql (database ACL, schema owner) and ANALYZE
# NOT done here: credentials, the anchor, the actor-proof re-key, `cmd/migrate verify`, the data comparison (verify.sh), reconciliation.
#
# Test hooks (drill only): RESTORE_TEST_FAULT=kill_midway terminates pg_restore part-way to prove the rollback; =swap_after_verify overwrites the
# originals after verification to prove only the verified private copies are used.
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

# ---- 1. authenticity first, on PRIVATE COPIES (A12 re-review SR12R-1) --------------------------------------------------
# What is verified must be exactly what is used: SHA256SUMS(.sig) and every listed artefact are copied into the private work directory, the signature and
# the checksums are checked on the COPIES, and only the copies are decrypted and restored. The backup directory may hold nothing else: an unlisted file
# (e.g. a plaintext db.dump beside an encrypted set), a symlink or a sub-directory is refused.
SETDIR="${WORK}/set"; mkdir -m 700 "$SETDIR"
while IFS= read -r -d '' e; do
  { [ -f "$e" ] && [ ! -L "$e" ]; } || bk_die "${e} in the backup directory is not a regular file (symlink / directory / special file); refusing"
done < <(find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -print0)
for f in SHA256SUMS SHA256SUMS.sig; do
  if [ -f "${BACKUP_DIR}/${f}" ] && [ ! -L "${BACKUP_DIR}/${f}" ]; then cp -- "${BACKUP_DIR}/${f}" "${SETDIR}/${f}"; fi
done
bk_verify_set_signature "$SETDIR" "$BACKUP_DIR" "$WORK"
SUMS_SHA="$(sha256sum <"${SETDIR}/SHA256SUMS" | cut -d' ' -f1)"
declare -A LISTED=()
while read -r h n; do
  n="${n#\*}"
  [[ "$h" =~ ^[0-9a-f]{64}$ ]] && [[ "$n" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || bk_die "the signed SHA256SUMS holds a malformed line; refusing"
  [ -z "${LISTED[$n]:-}" ] || bk_die "the signed SHA256SUMS lists ${n} twice; refusing"
  LISTED[$n]="$h"
done <"${SETDIR}/SHA256SUMS"
ENCEXT=""; ENCEXT_SET=0
for f in db.dump globals.sql baseline.txt; do
  hit=""; nhit=0
  for ext in "" .age .gpg; do if [ -n "${LISTED[${f}${ext}]:-}" ]; then hit="$ext"; nhit=$((nhit + 1)); fi; done
  [ "$nhit" = 1 ] || bk_die "the signed SHA256SUMS must list exactly one form of ${f} (found ${nhit}); refusing"
  if [ "$ENCEXT_SET" = 0 ]; then ENCEXT="$hit"; ENCEXT_SET=1; elif [ "$ENCEXT" != "$hit" ]; then bk_die "the signed set mixes encrypted and plaintext artefacts; refusing"; fi
done
[ -n "${LISTED[manifest.json]:-}" ] || bk_die "the signed SHA256SUMS does not list manifest.json; refusing"
[ "${#LISTED[@]}" = 4 ] || bk_die "the signed SHA256SUMS lists unexpected entries; refusing"
while IFS= read -r -d '' e; do
  n="${e##*/}"
  case "$n" in SHA256SUMS|SHA256SUMS.sig) continue ;; esac
  [ -n "${LISTED[$n]:-}" ] || bk_die "UNLISTED file '${n}' in the backup directory: it is not covered by the signed manifest; refusing (nothing was restored)"
done < <(find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -print0)
# DRILL-ONLY fault: turn an artefact into a symlink to attacker content AFTER the scan above (SR12R2-1). The copy below is by CONTENT, so the private copy is a regular
# file holding whatever was read, and that is what the checksum verifies.
if [ "${RESTORE_TEST_FAULT:-}" = "symlink_after_scan" ]; then
  [ "$BK_TARGET_LOCAL" -eq 1 ] || bk_die "RESTORE_TEST_FAULT is honoured only for a throwaway drill target"
  printf 'attacker content\n' >"${WORK}/attacker.json"
  rm -f -- "${BACKUP_DIR}/manifest.json"; ln -s "${WORK}/attacker.json" "${BACKUP_DIR}/manifest.json"
fi
# Copy by CONTENT (never a link: a copy that is a symlink back into the shared storage would be re-read from the attacker's file after verification, SR12R2-1).
# Each file is read once through a single open; the bytes that were read are what is checksummed and used.
for n in "${!LISTED[@]}"; do
  cat -- "${BACKUP_DIR}/${n}" >"${SETDIR}/${n}" 2>/dev/null || bk_die "CHECKSUM MISMATCH or missing file: ${n} listed in the signed manifest cannot be read from ${BACKUP_DIR}"
done
[ -z "$(find "$SETDIR" ! -type f ! -type d 2>/dev/null)" ] && [ -z "$(find "$SETDIR" -mindepth 1 -type d 2>/dev/null)" ] || bk_die "the private copy holds something other than regular files; refusing"
if ! ( cd "$SETDIR" && sha256sum --check --strict --quiet SHA256SUMS ) >"${WORK}/sum.out" 2>&1; then
  bk_die "CHECKSUM MISMATCH or missing file in ${BACKUP_DIR} - the backup is corrupted or tampered with; nothing was restored. $(head -3 "${WORK}/sum.out" | tr '\n' ' ')"
fi
# DRILL-ONLY fault: overwrite the originals in the backup directory AFTER verification. Only the private copies are used, so the restore must still produce the signed content.
if [ "${RESTORE_TEST_FAULT:-}" = "swap_after_verify" ]; then
  [ "$BK_TARGET_LOCAL" -eq 1 ] || bk_die "RESTORE_TEST_FAULT is honoured only for a throwaway drill target"
  for n in "${!LISTED[@]}" SHA256SUMS SHA256SUMS.sig; do printf 'EVIL' >"${BACKUP_DIR}/${n}"; done
fi
# freshness / identity of the set (SR12R-10): the operator compares this with the runner's out-of-band backup log
M_CREATED="$(sed -n 's/.*"created_utc": "\([0-9TZ]*\)".*/\1/p' "${SETDIR}/manifest.json")"
bk_log "signed set: created_utc=${M_CREATED:-?} SHA256SUMS sha256=${SUMS_SHA}"
if [ -n "${RESTORE_EXPECT_SET_SHA256:-}" ]; then
  [ "${RESTORE_EXPECT_SET_SHA256}" = "$SUMS_SHA" ] || bk_die "the set's SHA256SUMS hash (${SUMS_SHA}) does not equal RESTORE_EXPECT_SET_SHA256: this is not the set the backup runner reported (an older or substituted set); refusing"
elif [ "$BK_TARGET_LOCAL" -ne 1 ]; then
  bk_die "RESTORE_EXPECT_SET_SHA256 (the SHA-256 of SHA256SUMS from the backup runner's out-of-band log) is required for a non-drill target: it pins WHICH signed set may be restored (rollback protection)"
fi

# ---- 2. decrypt the private copies into the private work dir ---------------------------------------------------------
materialise() { # name -> path of the plaintext copy
  local f="$1"
  case "$ENCEXT" in
    "") printf '%s' "${SETDIR}/${f}" ;;
    .age)
      bk_need_tools age
      [ -n "${RESTORE_AGE_IDENTITY_FILE:-}" ] && [ -f "${RESTORE_AGE_IDENTITY_FILE}" ] || bk_die "backup is age-encrypted: RESTORE_AGE_IDENTITY_FILE (path to the private identity, from the secret store) is required"
      age -d -i "$RESTORE_AGE_IDENTITY_FILE" -o "${WORK}/${f}" "${SETDIR}/${f}.age" || bk_die "age decryption of ${f} failed (wrong identity?)"
      printf '%s' "${WORK}/${f}" ;;
    .gpg)
      bk_need_tools gpg
      gpg --batch --yes --quiet --output "${WORK}/${f}" --decrypt "${SETDIR}/${f}.gpg" || bk_die "gpg decryption of ${f} failed (key not available?)"
      printf '%s' "${WORK}/${f}" ;;
  esac
}
DUMP="$(materialise db.dump)"; GLOBALS="$(materialise globals.sql)"; BASELINE="$(materialise baseline.txt)"

# ---- 3. the signed manifest binds the decrypted bytes and the environment ---------------------------------------------
M_SHA="$(sed -n 's/.*"plaintext_dump_sha256": "\([0-9a-f]\{64\}\)".*/\1/p' "${SETDIR}/manifest.json")"
[ -n "$M_SHA" ] || bk_die "the signed manifest carries no plaintext_dump_sha256; refusing"
[ "$(sha256sum <"$DUMP" | cut -d' ' -f1)" = "$M_SHA" ] || bk_die "the decrypted dump does not match the signed manifest's plaintext_dump_sha256; refusing"
pg_restore --list "$DUMP" >"${WORK}/toc.txt" 2>"${WORK}/toc.err" || bk_die "pg_restore cannot read the dump (truncated or not a custom-format archive): $(head -2 "${WORK}/toc.err" | tr '\n' ' ')"
SRC_ENV="$(sed -n 's/.*"environment_anchor": "\([a-z]*\)".*/\1/p' "${SETDIR}/manifest.json")"
[ -n "$SRC_ENV" ] || bk_die "the signed manifest carries no source environment anchor; refusing to guess what is being restored"
[ "$(grep '^META|environment_anchor|' "$BASELINE" | cut -d'|' -f3- || true)" = "$SRC_ENV" ] || bk_die "the baseline's environment anchor differs from the signed manifest's ('${SRC_ENV}'); refusing"
[ "${RESTORE_EXPECT_SOURCE_ENVIRONMENT:-}" = "$SRC_ENV" ] \
  || bk_die "RESTORE_EXPECT_SOURCE_ENVIRONMENT must equal the backup's source environment anchor ('${SRC_ENV}'): state what you believe you are restoring; a mismatch is refused"
case "${RESTORE_TARGET_ENVIRONMENT:-}" in production|staging|development|test|unrecorded) ;; *) bk_die "RESTORE_TARGET_ENVIRONMENT (production|staging|development|test|unrecorded) is required: what environment is the restore target?" ;; esac
[ "$SRC_ENV" = "$RESTORE_TARGET_ENVIRONMENT" ] \
  || bk_die "the backup's source environment is '${SRC_ENV}' but the target environment is '${RESTORE_TARGET_ENVIRONMENT}': a restore is only allowed between the same environment (synthetic accounts and known test passwords must never enter another environment, and a copy must never be recorded as production)"

ME="$(bk_sql "SELECT current_user")"
case "$ME" in
  igaming_runtime|igaming_backup) bk_die "restoring as ${ME} is refused: the runtime role owns nothing and must never create objects; the backup role is SELECT-only. Use the restore role (the owner role, or a CREATEDB role that is a member of ${OWNER_ROLE})" ;;
esac
role_privs() { # role -> comma list of privileges that make a role dangerous as a restore identity or object owner (empty = none)
  bk_sql "SELECT concat_ws(',', CASE WHEN r.rolsuper THEN 'SUPERUSER' END, CASE WHEN r.rolcreaterole THEN 'CREATEROLE' END, CASE WHEN r.rolbypassrls THEN 'BYPASSRLS' END, CASE WHEN r.rolreplication THEN 'REPLICATION' END, (SELECT string_agg('member-of-' || g.rolname, ',' ORDER BY g.rolname) FROM pg_roles g WHERE ((g.rolname LIKE 'pg\_%' AND g.rolname <> 'pg_database_owner') OR g.rolname LIKE 'rds\_%' OR g.rolname = 'rdsadmin') AND g.oid <> r.oid AND pg_has_role(r.oid, g.oid, 'MEMBER'))) FROM pg_roles r WHERE r.rolname = '$1'"
}
role_sig() { # fingerprint of everything about roles that a load could change (no passwords: not readable by a plain role)
  bk_sql "SELECT md5(coalesce((SELECT string_agg(concat_ws('|', rolname, rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin, rolreplication, rolbypassrls, rolconnlimit, rolvaliduntil, coalesce(rolconfig::text, '')), ';' ORDER BY rolname) FROM pg_roles), '') || '#' || coalesce((SELECT string_agg(roleid::text || '>' || member::text || ':' || admin_option::text, ';' ORDER BY roleid, member) FROM pg_auth_members), '') || '#' || coalesce((SELECT string_agg(setdatabase::text || '/' || setrole::text || '=' || setconfig::text, ';' ORDER BY setdatabase, setrole) FROM pg_db_role_setting), ''))"
}
IFS='|' read -r R_SUPER R_CREATEDB R_MEMBER <<<"$(psql -X -q -At -F '|' -v ON_ERROR_STOP=1 -c "SELECT rolsuper::int, rolcreatedb::int, (current_user = '${OWNER_ROLE}' OR CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${OWNER_ROLE}') THEN pg_has_role(current_user, '${OWNER_ROLE}', 'MEMBER') ELSE false END)::int FROM pg_roles WHERE rolname = current_user")"
ME_PRIVS="$(role_privs "$ME")"
if [ "$PHASE" != provision ]; then
  # The archive runs with this session's rights (and, as the owner, may alter the owner role itself). So the loading identity must be a plain
  # NOSUPERUSER, NOCREATEROLE role with NO membership in pg_execute_server_program / pg_*_server_files / pg_*_all_data / pg_signal_backend /
  # rds_superuser or any other pg_* / rds_* role (A12 re-review SR12R-2).
  if [ -n "$ME_PRIVS" ]; then
    [ "${RESTORE_ALLOW_PRIVILEGED_DATA_ROLE:-}" = "drill-only" ] && [ "$BK_TARGET_LOCAL" -eq 1 ] \
      || bk_die "refusing to run the archive as ${ME}: it holds ${ME_PRIVS}, which would give a (possibly planted) archive entry those privileges (COPY ... TO PROGRAM, CREATE ROLE, ALTER ROLE, file access). Create the roles and the empty database with --phase provision, then run --phase load as the plain non-superuser owner role ${OWNER_ROLE}"
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
  OWN_PRIVS="$(role_privs "$OWNER_ROLE")"
  [ -z "$OWN_PRIVS" ] || bk_die "owner role ${OWNER_ROLE} holds ${OWN_PRIVS}: refusing to make it the owner of the restored objects (the archive runs as the owner and would inherit these privileges)"
  if [ "$OWNER_ROLE" != igaming ]; then bk_log "NOTICE: RESTORE_OWNER_ROLE_OVERRIDE in use: owner role ${OWNER_ROLE} instead of igaming"; fi
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
# Owner decision 5 (ADR 0122 section 10): the actor-proof signing secrets are NEVER restored either. A set made by this tool's backup.sh does not carry
# them (the table is dumped EMPTY); a LEGACY set (made before the decision) does, and is treated as a signing-material-bearing artefact: its
# actor_proof_keys rows are skipped here as well, so the restored database always starts with NO actor-proof key.
APK_LEGACY=0
grep -q -E ' TABLE DATA [^ ]+ actor_proof_key[^ ]* ' "${WORK}/toc.txt" && APK_LEGACY=1
grep -v -e ' TABLE DATA public database_environment ' "${WORK}/toc.txt" | grep -v -E ' TABLE DATA [^ ]+ actor_proof_key[^ ]* ' >"${WORK}/toc.filtered" || true
SKIPPED=$(( $(grep -c . "${WORK}/toc.txt") - $(grep -c . "${WORK}/toc.filtered") ))
if [ "$APK_LEGACY" -eq 1 ]; then
  bk_log "WARNING: this set CARRIES actor_proof_keys data (a legacy set made before owner decision 5). The rows are skipped, but the set itself holds signing secrets: handle and retire it as a signing-key-bearing artefact (docs/runbooks/backup-restore-dr.md section 5a) and rotate the actor-proof key family"
fi
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
ROLESIG0="$(role_sig)"
PGDATABASE="$NEW_DB" pg_restore --single-transaction --exit-on-error --no-owner --role="$OWNER_ROLE" -L "${WORK}/toc.filtered" \
  --dbname="$NEW_DB" "$DUMP" >"${WORK}/restore.out" 2>"${WORK}/restore.err" \
  || { bk_log "pg_restore failed: $(head -4 "${WORK}/restore.err" | tr '\n' ' ')"; exit 3; }
# The archive ran as the owner role and can alter ROLES, which are cluster-global and survive a rollback of the database. Detect what can be detected:
# attributes, per-role settings, memberships, and (when the connecting role is the owner) that its own credential still works. Anything else: the runbook
# requires re-provisioning every project credential after a restore (A12 re-review SR12R-2).
if [ "$ME" = "$OWNER_ROLE" ] && ! bk_sql "SELECT 1" >/dev/null 2>&1; then
  bk_log "CREDENTIAL CHANGE DETECTED: ${ME} can no longer connect with its credential after the load (the archive changed its password?). Treat as an incident; remove that new database by hand and rotate every project credential"
  exit 3
fi
if [ "$(role_sig)" != "$ROLESIG0" ]; then
  bk_log "ROLE CHANGE DETECTED: the archive changed role attributes, per-role settings or memberships during the load. Treat as an incident: rotate every project credential and inspect the cluster's roles; the restored database is dropped"
  exit 3
fi
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

bk_log "restored into ${NEW_DB} (owner ${OWNER_ROLE}); source environment '${SRC_ENV}'; the environment anchor was NOT restored and actor_proof_keys holds NO key (${SKIPPED} TOC data entries skipped)"
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
restore: ACTOR-PROOF KEY: the restored database holds NO actor-proof key (it is never in a logical dump); every governed write (K1/K2/K3, launch,
restore: RG floors, staff lifecycle) fails closed with AP002 and a production platform-api refuses to start until you provision a NEW key with
restore: deploy/backup/rekey-actor-proof.sh (never reuse the source's kid; verify.sh --expect-actor-proof provisioned then checks it).
EOF
