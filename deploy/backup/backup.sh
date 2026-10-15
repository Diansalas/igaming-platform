#!/usr/bin/env bash
# Logical backup of the platform database as the dedicated, audited backup role (ADR 0122, BACKUP-ROLE-1).
#
#   BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT=5499 BACKUP_PGDATABASE=igaming_platform_dev BACKUP_PGUSER=igaming_backup \
#   BACKUP_PGPASSFILE=/run/secrets/backup.pgpass deploy/backup/backup.sh --out-dir /var/backups/igaming
#
# What it produces (one directory per run, mode 0700, files 0600):
#   db.dump[.age|.gpg]   pg_dump -Fc of the whole database, taken in ONE exported REPEATABLE READ snapshot
#   globals.sql          pg_dumpall --globals-only --no-role-passwords (role attributes/grants; NO passwords)
#   baseline.txt         baseline.sql evaluated in the SAME snapshot (exact row counts + content hashes per table,
#                        ledger invariants, sequences, roles/grants, triggers, policies, catalog fingerprints)
#   SHA256SUMS           sha256 of every artifact above; checked by restore.sh BEFORE it touches any database
#   manifest.json        what/when/where, tool versions, sizes, timings, invariant status. No secret.
#
# Fail-closed behaviour:
#   * the connecting role must be NOSUPERUSER + BYPASSRLS (+ not the owner/runtime role): a plain owner or runtime
#     dump is silently incomplete or refused under FORCE RLS (BACKUP-ROLE-1) - that is refused here with an explanation;
#   * an unencrypted dump is refused unless BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only is set (a real dump holds
#     PII, financial data and the actor-proof signing keys). Encryption hook: BACKUP_ENCRYPT_AGE_RECIPIENT=<age public
#     recipient> or BACKUP_ENCRYPT_GPG_RECIPIENT=<gpg key id/fingerprint>. This repository holds NO key material;
#     the recipient is a public reference, the private key lives with the restorer's secret store / KMS;
#   * any failure removes the partial output directory;
#   * source ledger/projection invariants are checked on the snapshot; a violation is recorded and the script exits 4
#     (the artifacts are kept and marked, they are evidence).
#
# Exit: 0 ok; 2 refused/usage/preflight; 3 backup failed; 4 backup written but SOURCE INVARIANTS VIOLATED.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"

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

bk_need_tools psql pg_dump pg_dumpall sha256sum awk
bk_connect_env
umask 077

# ---- preflight: who are we, may we do this? --------------------------------------------------------------
if ! PRE="$(psql -X -q -At -v ON_ERROR_STOP=1 -F '|' -c "SELECT rolsuper::int, rolbypassrls::int, rolcreaterole::int, rolcreatedb::int, rolreplication::int, current_setting('server_version_num')::int FROM pg_roles WHERE rolname = current_user" 2>"${TMPDIR:-/tmp}/bk-pre.$$")"; then
  msg="$(head -2 "${TMPDIR:-/tmp}/bk-pre.$$" | tr '\n' ' ')"; rm -f "${TMPDIR:-/tmp}/bk-pre.$$"
  bk_die "cannot connect as ${BACKUP_PGUSER} to ${BACKUP_PGHOST}:${BACKUP_PGPORT}/${BACKUP_PGDATABASE}: ${msg} (typical causes: the time-boxed backup credential EXPIRED - re-mint it with init-backup-role.sql / the secret store job; wrong passfile; role missing; host not allowed)"
fi
rm -f "${TMPDIR:-/tmp}/bk-pre.$$"
IFS='|' read -r R_SUPER R_BYPASS R_CREATEROLE R_CREATEDB R_REPL R_VERNUM <<<"$PRE"
[ "$R_SUPER" = 0 ] || bk_die "refusing to back up as a SUPERUSER (${BACKUP_PGUSER}); use the dedicated NOSUPERUSER BYPASSRLS backup role (ADR 0122)"
[ "$R_BYPASS" = 1 ] || bk_die "role ${BACKUP_PGUSER} does not have BYPASSRLS: under FORCE ROW LEVEL SECURITY a dump by this role is refused or silently INCOMPLETE (BACKUP-ROLE-1). Use the dedicated backup role (deploy/backup/init-backup-role.sql); not weakening RLS to dump"
[ "$R_CREATEROLE" = 0 ] && [ "$R_CREATEDB" = 0 ] && [ "$R_REPL" = 0 ] || bk_die "role ${BACKUP_PGUSER} holds CREATEROLE/CREATEDB/REPLICATION; the backup role must have none of them"
RO="$(bk_sql "SELECT current_setting('default_transaction_read_only')")"
[ "$RO" = on ] || bk_die "backup role session is not read-only by default (default_transaction_read_only=${RO}); re-run init-backup-role.sql"
MISSING="$(bk_sql "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public' WHERE c.relkind IN ('r','p','S') AND NOT has_table_privilege(c.oid, 'SELECT')")"
[ "$MISSING" = 0 ] || bk_die "backup role lacks SELECT on ${MISSING} table(s)/sequence(s) (a new migration table without the default privilege?); re-run init-backup-role.sql. Refusing to produce an incomplete dump"

ENC=none
if [ -n "${BACKUP_ENCRYPT_AGE_RECIPIENT:-}" ] && [ -n "${BACKUP_ENCRYPT_GPG_RECIPIENT:-}" ]; then bk_die "set only one of BACKUP_ENCRYPT_AGE_RECIPIENT / BACKUP_ENCRYPT_GPG_RECIPIENT"; fi
if [ -n "${BACKUP_ENCRYPT_AGE_RECIPIENT:-}" ]; then bk_need_tools age; ENC=age
elif [ -n "${BACKUP_ENCRYPT_GPG_RECIPIENT:-}" ]; then bk_need_tools gpg; ENC=gpg
elif [ "${BACKUP_ALLOW_UNENCRYPTED:-}" != "synthetic-data-only" ]; then
  bk_die "no encryption recipient configured. A real dump contains PII, financial data and signing keys: set BACKUP_ENCRYPT_AGE_RECIPIENT or BACKUP_ENCRYPT_GPG_RECIPIENT (public reference only). For SYNTHETIC drill data only: BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only"
fi

MIN_FREE_MB="${BACKUP_MIN_FREE_MB:-512}"
mkdir -p "$OUT_DIR"
FREE_MB="$(df -Pm "$OUT_DIR" | awk 'NR==2 {print $4}')"
[ "$FREE_MB" -ge "$MIN_FREE_MB" ] || bk_die "insufficient disk space in ${OUT_DIR}: ${FREE_MB} MB free, ${MIN_FREE_MB} MB required (BACKUP_MIN_FREE_MB). A dump needs roughly the compressed database size plus a copy for checksumming; nothing was written"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${OUT_DIR}/${STAMP}-${BACKUP_PGDATABASE}${LABEL:+-$LABEL}"
[ ! -e "$RUN_DIR" ] || bk_die "refusing to overwrite existing ${RUN_DIR}"
mkdir -m 700 "$RUN_DIR"
SESSION_PID=""
fail_cleanup() {
  local rc=$?
  if [ -n "$SESSION_PID" ]; then kill "$SESSION_PID" 2>/dev/null || true; fi
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 4 ]; then
    rm -rf "$RUN_DIR"
    echo "backup: FAILED (exit ${rc}); partial output ${RUN_DIR} removed" >&2
  fi
}
trap fail_cleanup EXIT

T0="$(now_s)"

# ---- one exported snapshot shared by pg_dump and the baseline queries --------------------------------------
FIFO_DIR="$(mktemp -d "${RUN_DIR}/.sess.XXXXXX")"
mkfifo "${FIFO_DIR}/in" "${FIFO_DIR}/out"
psql -X -q -At -v ON_ERROR_STOP=1 <"${FIFO_DIR}/in" >"${FIFO_DIR}/out" 2>"${FIFO_DIR}/err" &
SESSION_PID=$!
exec 7>"${FIFO_DIR}/in" 8<"${FIFO_DIR}/out"
echo "BEGIN ISOLATION LEVEL REPEATABLE READ, READ ONLY;" >&7
echo "SELECT pg_export_snapshot();" >&7
IFS= read -r SNAP <&8 || bk_die "could not export a snapshot: $(head -2 "${FIFO_DIR}/err" | tr '\n' ' ')"
[ -n "$SNAP" ] || bk_die "empty snapshot id"

# baseline evaluated inside the snapshot (the file's output is written by this very session)
printf '\\o %s\n\\i %s\n\\o\nSELECT %s;\n' "${RUN_DIR}/baseline.txt" "${SCRIPT_DIR}/baseline.sql" "'__BASELINE_DONE__'" >&7
IFS= read -r DONE <&8 || true
[ "$DONE" = "__BASELINE_DONE__" ] || bk_die "baseline queries failed: $(head -3 "${FIFO_DIR}/err" | tr '\n' ' ')"
T_BASE="$(now_s)"

# the dump itself, in the same snapshot
pg_dump -Fc --snapshot="$SNAP" --file="${RUN_DIR}/db.dump" 2>"${RUN_DIR}/.pg_dump.err" || {
  bk_die "pg_dump failed: $(head -3 "${RUN_DIR}/.pg_dump.err" | tr '\n' ' ')"
}
T_DUMP="$(now_s)"
echo "COMMIT;" >&7
exec 7>&- 8<&-
wait "$SESSION_PID" 2>/dev/null || true
SESSION_PID=""
rm -rf "$FIFO_DIR" "${RUN_DIR}/.pg_dump.err"

# role/grant definitions (no passwords: pg_dumpall without role-password access needs only pg_roles)
pg_dumpall --globals-only --no-role-passwords --database="${BACKUP_PGDATABASE}" --file="${RUN_DIR}/globals.sql" 2>"${RUN_DIR}/.globals.err" \
  || bk_die "pg_dumpall --globals-only failed: $(head -3 "${RUN_DIR}/.globals.err" | tr '\n' ' ')"
rm -f "${RUN_DIR}/.globals.err"
T_GLOB="$(now_s)"

# ---- invariants on the snapshot (read from the plaintext baseline BEFORE any encryption) ---------------------
INV_BAD="$(awk -F'|' '$1=="INV" && $3!="0" {n++} END {print n+0}' "${RUN_DIR}/baseline.txt")"
TABLES="$(grep -c '^TABLE|' "${RUN_DIR}/baseline.txt" || true)"
HEAD_LINE="$(grep '^META|migration_head|' "${RUN_DIR}/baseline.txt" | cut -d'|' -f3- || true)"

# ---- encryption hook ----------------------------------------------------------------------------------------
PLAIN_SHA="$(sha256sum "${RUN_DIR}/db.dump" | cut -d' ' -f1)"
case "$ENC" in
  age) age -r "$BACKUP_ENCRYPT_AGE_RECIPIENT" -o "${RUN_DIR}/db.dump.age" "${RUN_DIR}/db.dump" && rm -f "${RUN_DIR}/db.dump"
       age -r "$BACKUP_ENCRYPT_AGE_RECIPIENT" -o "${RUN_DIR}/globals.sql.age" "${RUN_DIR}/globals.sql" && rm -f "${RUN_DIR}/globals.sql"
       age -r "$BACKUP_ENCRYPT_AGE_RECIPIENT" -o "${RUN_DIR}/baseline.txt.age" "${RUN_DIR}/baseline.txt" && rm -f "${RUN_DIR}/baseline.txt" ;;
  gpg) for f in db.dump globals.sql baseline.txt; do
         gpg --batch --yes --quiet --trust-model always --recipient "$BACKUP_ENCRYPT_GPG_RECIPIENT" --output "${RUN_DIR}/${f}.gpg" --encrypt "${RUN_DIR}/${f}" && rm -f "${RUN_DIR}/${f}"
       done ;;
esac
T_ENC="$(now_s)"

# ---- checksums + manifest -------------------------------------------------------------------------------------
( cd "$RUN_DIR" && sha256sum -- db.dump* globals.sql* baseline.txt* > SHA256SUMS )
DUMP_BYTES="$(stat -c %s "${RUN_DIR}"/db.dump*)"
GIT_COMMIT="$(git -C "${SCRIPT_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)"
cat >"${RUN_DIR}/manifest.json" <<EOF
{
  "format": "igaming-logical-backup/1",
  "created_utc": "${STAMP}",
  "database": "${BACKUP_PGDATABASE}",
  "host_ref": "${BACKUP_PGHOST}:${BACKUP_PGPORT}",
  "role": "${BACKUP_PGUSER}",
  "server_version_num": ${R_VERNUM},
  "pg_dump": "$(pg_dump --version | tr -d '\n')",
  "migration_head": "${HEAD_LINE}",
  "tables_in_baseline": "${TABLES}",
  "encryption": "${ENC}",
  "plaintext_dump_sha256": "${PLAIN_SHA}",
  "dump_bytes": ${DUMP_BYTES},
  "source_invariant_violations": "${INV_BAD}",
  "repo_commit": "${GIT_COMMIT}",
  "seconds": { "baseline_queries": $(elapsed_s "$T0" "$T_BASE"), "pg_dump": $(elapsed_s "$T_BASE" "$T_DUMP"), "globals": $(elapsed_s "$T_DUMP" "$T_GLOB"), "encrypt": $(elapsed_s "$T_GLOB" "$T_ENC"), "total": $(elapsed_s "$T0" "$(now_s)") }
}
EOF
chmod 600 "${RUN_DIR}"/*
bk_log "backup written: ${RUN_DIR}"
echo "RUN_DIR=${RUN_DIR}"
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
