#!/usr/bin/env bash
# A12: REAL local backup / restore / point-in-time-recovery drill with synthetic data (ADR 0122).
#
#   deploy/backup/drill.sh [--tenants N] [--players N] [--rounds N] [--small] [--skip-pitr] [--results-file FILE]
#
# Builds its OWN throwaway PostgreSQL 16 clusters under /tmp/pgdrill-XXXXXX (ports 5499 source, 5498 full-loss
# restore target, 5497 PITR recovery), runs everything against them, and tears them down on exit/failure. It
# never contacts, configures or reads the shared development/CI cluster on 5432, creates roles only inside the
# throwaway clusters, uses no sudo/pkill (`runuser -u postgres` drops privileges; clusters are stopped with pg_ctl),
# touches no network or AWS, and uses only synthetic data and per-run random passwords.
#
# Phases (RESULT lines; exit 0 only if all PASS):
#   0 preflight (tools, disk)      1 cluster + roles (deploy/init-app-role.sql) + migrations + seed through the platform
#   2 BACKUP-ROLE-1 proof          3 logical backup (plain + encrypted hook) -> restore -> verify (same cluster)
#   4 full-loss restore into a NEW cluster -> credentials re-provisioned -> anchor behaviour -> verify
#   5 negative control + failure handling drills     6 PITR (deploy/backup/pitr-drill.sh)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"
# shellcheck source=deploy/backup/lib-cluster.sh
. "${SCRIPT_DIR}/lib-cluster.sh"
# shellcheck source=deploy/backup/lib-provision.sh
. "${SCRIPT_DIR}/lib-provision.sh"

TENANTS=4; PLAYERS=24; ROUNDS=400; DEPOSITS=3; SKIP_PITR=0; RESULTS_FILE=""; MIN_ENTRIES=100000; SMALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --tenants) shift; TENANTS="${1:-}" ;;
    --players) shift; PLAYERS="${1:-}" ;;
    --rounds) shift; ROUNDS="${1:-}" ;;
    --small) SMALL=1; TENANTS=2; PLAYERS=5; ROUNDS=30; MIN_ENTRIES=0 ;;
    --skip-pitr) SKIP_PITR=1 ;;
    --results-file) shift; RESULTS_FILE="${1:-}" ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 0 ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done

bk_need_tools runuser psql pg_dump pg_restore pg_dumpall sha256sum go awk age age-keygen openssl
for t in pg_basebackup pg_verifybackup; do [ -x "${PG_BIN}/${t}" ] || bk_die "${PG_BIN}/${t} not found"; done
bk_check_free() { # min_mb phase
  local free; free="$(df -Pm /tmp | awk 'NR==2 {print $4}')"
  [ "$free" -ge "$1" ] || bk_die "insufficient disk in /tmp before ${2}: ${free} MB free, ${1} MB needed. The drill stops here (and tears down) rather than risk filling the shared disk"
}
bk_check_free "${DRILL_MIN_FREE_MB:-900}" "start"

trap cl_teardown EXIT INT TERM
cl_new_root
WORK="${DRILL_ROOT}/work"; SECRETS="${DRILL_ROOT}/secrets"; BKUP="${DRILL_ROOT}/backups"
mkdir -p "$BKUP"; chmod 700 "$BKUP"

OUT="${WORK}/results.txt"; : >"$OUT"
FAILS=0
res() { echo "RESULT|$1|$2|$3" | tee -a "$OUT"; [ "$2" = PASS ] || FAILS=$((FAILS + 1)); }
meas() { echo "$*" | tee -a "$OUT"; }
section() { echo; echo "=== $* ==="; }

DB=igaming_platform_dev
SRC_PORT=5499; LOSS_PORT=5498
# psql as a role (passfile based) against a throwaway port
psql_as() { # user passfile port db args...
  local u="$1" pf="$2" p="$3" d="$4"; shift 4
  PGPASSFILE="$pf" psql -X -q -At -h 127.0.0.1 -p "$p" -U "$u" -d "$d" "$@"
}
pw_of() { awk -F: -v p="$2" -v u="$3" '$2==p && $4==u {print $5}' "$1"; }

# ================================================================= phase 1: cluster, roles, migrations, seed
section "1 cluster, roles, migrations, synthetic data (platform services)"
echo "building binaries"
( cd "$REPO_ROOT" && go build -o "${WORK}/migrate" ./cmd/migrate && go build -o "${WORK}/restorecheck" ./deploy/backup/restorecheck && go build -o "${WORK}/scramhash" ./deploy/backup/scramhash )
export SCRAMHASH_BIN="${WORK}/scramhash"
# controlled temp root for the scripts (H2: private, inside the drill root, swept at start)
export BACKUP_TMPDIR="${DRILL_ROOT}/tmp"; mkdir -p "$BACKUP_TMPDIR"; chmod 700 "$BACKUP_TMPDIR"
# EPHEMERAL signing identity (H3): the private key never enters the backup directory; the public key is pinned by fingerprint
openssl genpkey -algorithm ED25519 -out "${SECRETS}/sign.pem" 2>/dev/null; chmod 600 "${SECRETS}/sign.pem"
openssl pkey -in "${SECRETS}/sign.pem" -pubout -out "${SECRETS}/sign.pub" 2>/dev/null
openssl genpkey -algorithm ED25519 -out "${SECRETS}/attacker.pem" 2>/dev/null; chmod 600 "${SECRETS}/attacker.pem"
openssl pkey -in "${SECRETS}/attacker.pem" -pubout -out "${SECRETS}/attacker.pub" 2>/dev/null
SIGN_FP="$(openssl pkey -pubin -in "${SECRETS}/sign.pub" -outform DER | sha256sum | cut -d' ' -f1)"
ATTACKER_FP="$(openssl pkey -pubin -in "${SECRETS}/attacker.pub" -outform DER | sha256sum | cut -d' ' -f1)"
export BACKUP_SIGNING_KEY_FILE="${SECRETS}/sign.pem" RESTORE_VERIFY_PUBKEY_FILE="${SECRETS}/sign.pub" RESTORE_VERIFY_PUBKEY_SHA256="$SIGN_FP"
export RESTORE_EXPECT_SOURCE_ENVIRONMENT=development RESTORE_TARGET_ENVIRONMENT=development
# rehash_manifest <dir> : set the manifest's plaintext_dump_sha256 to the CURRENT db.dump (what a compromised pipeline that holds the signing key would do; callers resign afterwards)
rehash_manifest() { sed -i "s/\"plaintext_dump_sha256\": \"[0-9a-f]*\"/\"plaintext_dump_sha256\": \"$(sha256sum <"$1/db.dump" | cut -d' ' -f1)\"/" "$1/manifest.json"; }
# build_trap_archive <outfile> <plpgsql statement> : a custom-format archive whose CHECK-constraint function runs <statement> while the rows are loaded by a restore
build_trap_archive() {
  local outfile="$1" body="$2"
  cl_super src "$SRC_PORT" -d postgres -c "CREATE DATABASE trapsrc OWNER igaming TEMPLATE template0" >/dev/null
  cl_super src "$SRC_PORT" -d trapsrc >/dev/null <<SQL
SET ROLE igaming;
CREATE FUNCTION evil(int) RETURNS int LANGUAGE plpgsql VOLATILE AS \$f\$ BEGIN RETURN \$1; END \$f\$;
CREATE TABLE trap_t (a int CONSTRAINT trap_chk CHECK (evil(a) > 0));
INSERT INTO trap_t VALUES (1);
CREATE OR REPLACE FUNCTION evil(int) RETURNS int LANGUAGE plpgsql VOLATILE AS \$f\$ BEGIN ${body}; RETURN \$1; END \$f\$;
SQL
  pg_dump -h "$(cl_sock src)" -p "$SRC_PORT" -U postgres -Fc -f "$outfile" trapsrc
  cl_super src "$SRC_PORT" -d postgres -c "DROP DATABASE trapsrc WITH (FORCE)" >/dev/null
}
# resign <dir> [key] : recompute SHA256SUMS over the artefacts and sign it (a legitimate re-seal: used to build tamper fixtures)
resign() { ( cd "$1" && sha256sum -- db.dump* globals.sql* baseline.txt* manifest.json >SHA256SUMS ) && openssl pkeyutl -sign -rawin -inkey "${2:-${SECRETS}/sign.pem}" -in "$1/SHA256SUMS" -out "$1/SHA256SUMS.sig" 2>/dev/null; }
# bulk load with archiving OFF (keeps /tmp bounded: ~15 KB of WAL per ledger entry); archiving is enabled before the
# first backup, so the PITR window starts at the base backup (documented limit of this drill)
cl_init src "$SRC_PORT" "archive_timeout = 30
max_wal_size = 128MB
min_wal_size = 48MB"
cl_start src "$SRC_PORT"
prov_roles src "$SRC_PORT"
T=$(now_s)
DATABASE_URL="postgres://igaming:$(pw_of "${SECRETS}/pgpass" "$SRC_PORT" igaming)@127.0.0.1:${SRC_PORT}/${DB}?sslmode=disable" "${WORK}/migrate" -dir "${REPO_ROOT}/migrations" up >"${WORK}/migrate.log" 2>&1
meas "TIMING|migrate_up_seconds|$(elapsed_s "$T" "$(now_s)")"
prov_reapply_app_roles src "$SRC_PORT"
# this is a DEVELOPMENT database: record that in the non-forgeable anchor (the backup carries the source environment; M4)
cl_super src "$SRC_PORT" -d "$DB" -c "SET ROLE igaming; INSERT INTO database_environment (environment) VALUES ('development')" >/dev/null
FUTURE="$(date -u -d '+1 hour' '+%Y-%m-%d %H:%M:%S+00')"
prov_backup_role src "$SRC_PORT" "$FUTURE"
prov_restore_role src "$SRC_PORT"
prov_replication_role src "$SRC_PORT"
prov_harden src "$SRC_PORT"
chmod 600 "${SECRETS}"/pgpass*
res cluster_and_roles PASS "throwaway cluster ${DRILL_ROOT}/src on 127.0.0.1:${SRC_PORT}; roles igaming, igaming_runtime (deploy/init-app-role.sql), igaming_backup (client-side SCRAM verifier), drill_restore, drill_replicator; harden-backup-surface.sql applied; $(grep -c . "${WORK}/migrate.log") migrate log lines"

bk_check_free 600 "seeding"
T=$(now_s)
DRILL_SEED_TENANTS="$TENANTS" DRILL_SEED_PLAYERS="$PLAYERS" DRILL_SEED_ROUNDS="$ROUNDS" DRILL_SEED_DEPOSITS="$DEPOSITS" DRILL_SEED_WORKERS="${DRILL_SEED_WORKERS:-8}" \
  "${SCRIPT_DIR}/seed.sh" --drill-root "$DRILL_ROOT" --port "$SRC_PORT" --report "${WORK}/seed.json" >"${WORK}/seed.log" 2>&1 \
  || { tail -15 "${WORK}/seed.log" >&2; bk_die "seeding failed"; }
meas "TIMING|seed_seconds|$(elapsed_s "$T" "$(now_s)")"
BPF="${SECRETS}/pgpass.backup"
src_q() { psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "$1"; }
ENTRIES="$(src_q 'SELECT count(*) FROM ledger_entries')"
TXNS="$(src_q 'SELECT count(*) FROM ledger_transactions')"
TOTAL_ROWS="$(src_q "SELECT sum((xpath('/row/n/text()', query_to_xml(format('select count(*) as n from public.%I', relname), false, true, '')))[1]::text::bigint) FROM pg_class WHERE relkind IN ('r','p') AND relnamespace = 'public'::regnamespace")"
DBSIZE="$(src_q "SELECT pg_database_size(current_database())")"
meas "SIZE|source_ledger_entries|${ENTRIES}"; meas "SIZE|source_ledger_transactions|${TXNS}"; meas "SIZE|source_total_rows|${TOTAL_ROWS}"; meas "SIZE|source_database_bytes|${DBSIZE}"
meas "DATA|seed_tally|$(tr -d '\n ' <"${WORK}/seed.json")"
meas "DATA|ledger_transaction_types|$(src_q "SELECT string_agg(transaction_type || '=' || n, ', ' ORDER BY n DESC) FROM (SELECT transaction_type, count(*) n FROM ledger_transactions GROUP BY 1) q")"
meas "DATA|withdrawal_states|$(src_q "SELECT string_agg(state || '=' || n, ', ') FROM (SELECT state, count(*) n FROM withdrawal_requests GROUP BY 1) q")"
meas "DATA|adjustment_states|$(src_q "SELECT string_agg(state || '=' || n, ', ') FROM (SELECT state, count(*) n FROM ledger_adjustment_requests GROUP BY 1) q")"
meas "DATA|audit_rows_alerts_bonus_grants|$(src_q "SELECT (SELECT count(*) FROM audit_log) || ' / ' || (SELECT count(*) FROM alerts) || ' / ' || (SELECT count(*) FROM bonus_grants)")"
if [ "$ENTRIES" -ge "$MIN_ENTRIES" ]; then res synthetic_world_loaded_through_platform_services PASS "${ENTRIES} ledger entries in ${TXNS} transactions, ${TOTAL_ROWS} rows in the database (target >= ${MIN_ENTRIES})"; else res synthetic_world_loaded_through_platform_services FAIL "only ${ENTRIES} ledger entries (< ${MIN_ENTRIES})"; fi

# ---- owner decision 5 (ADR 0122 section 10): plant a recognisable CANARY actor-proof secret next to the platform key the seed provisioned --------------------
# The canary is a high-entropy marker with a fixed prefix; it exists only in this throwaway cluster and in ${WORK}/canary.patterns (all forms a leak could take:
# the ASCII bytes, their hex (a bytea text literal) and their base64). The platform key the seed provisioned (random bytes) is hunted the same way.
OPF="${SECRETS}/pgpass"
CANARY="CANARY-ACTOR-PROOF-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
CANARY_KID=ap-canary-src
CANARY_HEX="$(printf '%s' "$CANARY" | od -An -tx1 | tr -d ' \n')"
IGPW="$CANARY" CANARY_KID_V="$CANARY_KID" cl_super src "$SRC_PORT" -d "$DB" <<'SQL' >/dev/null
\getenv cn IGPW
\getenv ck CANARY_KID_V
SET ROLE igaming;
INSERT INTO actorproof_private.actor_proof_keys (kid, secret) VALUES (:'ck', convert_to(:'cn', 'UTF8'));
SQL
SEED_AP_HEX="$(psql_as igaming "$OPF" "$SRC_PORT" "$DB" -c "SELECT encode(secret, 'hex') FROM actorproof_private.actor_proof_keys WHERE kid <> '${CANARY_KID}' ORDER BY created_at LIMIT 1")"
SRC_AP_KIDS="$(psql_as igaming "$OPF" "$SRC_PORT" "$DB" -c "SELECT string_agg(kid, ' ' ORDER BY kid) FROM actorproof_private.actor_proof_keys")"   # the owner: the backup role cannot see even the kids (F1)
SEED_AP_KID="$(printf '%s' "$SRC_AP_KIDS" | tr ' ' '\n' | grep -vx "$CANARY_KID" | head -1)"
{
  printf '%s\n' "$CANARY" "$CANARY_HEX" "$(printf '%s' "$CANARY" | base64 -w0)"
  if [ -n "$SEED_AP_HEX" ]; then
    SEED_ESC="$(printf '%s' "$SEED_AP_HEX" | sed 's/../\\x&/g')"
    # shellcheck disable=SC2059
    printf '%s\n' "$SEED_AP_HEX" "$(printf "$SEED_ESC" | base64 -w0)"
  fi
} >"${WORK}/canary.patterns"
chmod 600 "${WORK}/canary.patterns"
SRC_AP_ROWS="$(psql_as igaming "$OPF" "$SRC_PORT" "$DB" -c "SELECT count(*) FROM actorproof_private.actor_proof_keys")"
if [ "$SRC_AP_ROWS" -ge 2 ] && [ -n "$SEED_AP_HEX" ] && [ "$(wc -l <"${WORK}/canary.patterns")" = 5 ]; then res source_holds_actor_proof_secrets_to_hunt PASS "the source has ${SRC_AP_ROWS} actor_proof_keys rows (kids: ${SRC_AP_KIDS}): the seed's platform key plus a planted canary; 5 search patterns (canary ASCII/hex/base64, platform key hex/base64) are held in the work dir"; else res source_holds_actor_proof_secrets_to_hunt FAIL "rows=${SRC_AP_ROWS} seed_hex_len=${#SEED_AP_HEX} kids=${SRC_AP_KIDS}"; fi
# leak counter: occurrences of any pattern (-F fixed strings, -a binary as text) in the given files / stdin
leaks() { { grep -a -h -c -F -f "${WORK}/canary.patterns" "$@" 2>/dev/null || true; } | awk '{n += $1} END {print n+0}'; }

# ================================================================= phase 2: BACKUP-ROLE-1 proof
section "2 BACKUP-ROLE-1: the problem, and the dedicated role"
OPF="${SECRETS}/pgpass"
dump_as() { PGPASSFILE="$2" pg_dump -h 127.0.0.1 -p "$SRC_PORT" -U "$1" -Fc "${@:3}" "$DB"; }
if OUTP="$(dump_as igaming "$OPF" -f /dev/null 2>&1)"; then res owner_plain_dump_refused FAIL "owner pg_dump unexpectedly succeeded"; else
  case "$OUTP" in *"row-level security"*) res owner_plain_dump_refused PASS "pg_dump as the migration owner FAILS: $(printf '%s' "$OUTP" | head -1 | cut -c1-150)" ;; *) res owner_plain_dump_refused FAIL "failed for another reason: $OUTP" ;; esac; fi
if dump_as igaming "$OPF" --enable-row-security -f "${WORK}/owner-rls.dump" >/dev/null 2>"${WORK}/owner-rls.err"; then
  OWNER_ROWS="$(pg_restore -a -t ledger_entries -f - "${WORK}/owner-rls.dump" 2>/dev/null | awk '/^COPY /{f=1;next} /^\\\.$/{f=0} f{n++} END{print n+0}' || true)"
  if [ "${OWNER_ROWS:-0}" -lt "$ENTRIES" ]; then res owner_dump_with_enable_row_security_is_incomplete PASS "pg_dump --enable-row-security as the owner SUCCEEDS but holds ${OWNER_ROWS} of ${ENTRIES} ledger_entries (silent data loss; this is why it is not a backup)"; else res owner_dump_with_enable_row_security_is_incomplete FAIL "owner dump held ${OWNER_ROWS} rows"; fi
else
  res owner_dump_with_enable_row_security_is_incomplete PASS "pg_dump --enable-row-security as the owner also fails: $(head -1 "${WORK}/owner-rls.err" | cut -c1-150)"
fi
rm -f "${WORK}/owner-rls.dump"
if OUTP="$(dump_as igaming_runtime "$OPF" -f /dev/null 2>&1)"; then res runtime_role_dump_refused FAIL "runtime role dump unexpectedly succeeded"; else res runtime_role_dump_refused PASS "pg_dump as igaming_runtime FAILS: $(printf '%s' "$OUTP" | head -1 | cut -c1-120)"; fi
# the backup role: complete and read-only
dump_as igaming_backup "$BPF" --exclude-schema=actorproof_private -f "${WORK}/probe.dump" >/dev/null 2>&1 && PROBE_ROWS="$(pg_restore -a -t ledger_entries -f - "${WORK}/probe.dump" | awk '/^COPY /{f=1;next} /^\\\.$/{f=0} f{n++} END{print n+0}')" || PROBE_ROWS=-1
rm -f "${WORK}/probe.dump"
if [ "$PROBE_ROWS" = "$ENTRIES" ]; then res backup_role_dump_complete PASS "pg_dump as igaming_backup holds all ${PROBE_ROWS} ledger_entries"; else res backup_role_dump_complete FAIL "backup role dump rows=${PROBE_ROWS} expected ${ENTRIES}"; fi
write_denied=0; write_total=0
for stmt in "INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES (gen_random_uuid(), 'x', 'x', 'under_platform_licence', 'active')" \
            "UPDATE ledger_entries SET amount = amount + 1" "DELETE FROM ledger_entries" "TRUNCATE ledger_entries" \
            "CREATE TABLE zz_backup_probe (a int)" "CREATE TEMP TABLE zz_probe (a int)" "DROP TABLE ledger_entries" \
            "ALTER TABLE ledger_entries DISABLE TRIGGER ALL" \
            "SET ROLE igaming" "SET session_replication_role = replica" "CREATE ROLE zz_probe" "ALTER ROLE igaming_backup SUPERUSER" \
            "COPY (SELECT 1) TO PROGRAM 'true'" "SELECT pg_read_file('/etc/passwd')" "SELECT pg_reload_conf()"; do
  write_total=$((write_total + 1))
  if psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "BEGIN READ WRITE; ${stmt}; COMMIT" >/dev/null 2>"${WORK}/probe.err"; then echo "NOT DENIED: ${stmt}" >>"${WORK}/probe.fail"; else write_denied=$((write_denied + 1)); fi
done
if [ "$write_denied" = "$write_total" ]; then res backup_role_cannot_write_or_escalate PASS "${write_total}/${write_total} probes denied (DML, DDL, TRUNCATE, TEMP, trigger disable, SET ROLE, replica mode, CREATE/ALTER ROLE, COPY PROGRAM, file read, reload conf)"; else res backup_role_cannot_write_or_escalate FAIL "$(cat "${WORK}/probe.fail" | tr '\n' ';')"; fi
# ---- F1 (migration 0166, owner decision 5; SRBX-3 / residual R1): the backup CREDENTIAL cannot read the actor-proof signing key -------------------
# The key table lives in the owner-only schema actorproof_private. Probed as igaming_backup: the 3 key probes of the staging runner's pack
# (deploy/staging-runner/lib-probes.sh RN_KEY_PROBES) plus three more, each judged by SQLSTATE (only 42501 passes); the search_path detour; pg_dump of the
# schema, of the table and of the whole database WITHOUT the exclusion (each must fail and print no secret); the privilege functions by OID; and no
# SECURITY DEFINER function that reaches into the schema is executable by the role (it cannot learn even the key ids).
# Positive control: the cluster superuser's dump of the same schema DOES carry the planted canary and the platform key (the hunt is not blind).
key_denied=0; key_total=0; : >"${WORK}/key.fail"
for stmt in "SELECT secret FROM actorproof_private.actor_proof_keys" "LOCK TABLE actorproof_private.actor_proof_keys IN ACCESS SHARE MODE" \
            "CREATE TABLE actorproof_private.zz_backup_probe (a int)" "SELECT kid FROM actorproof_private.actor_proof_keys" \
            "ALTER TABLE actorproof_private.actor_proof_keys OWNER TO igaming_backup" "GRANT USAGE ON SCHEMA actorproof_private TO igaming_backup"; do
  key_total=$((key_total + 1))
  if psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -v VERBOSITY=sqlstate -c "BEGIN READ WRITE; ${stmt}; ROLLBACK" >"${WORK}/key.out" 2>"${WORK}/key.err"; then echo "NOT DENIED: ${stmt}" >>"${WORK}/key.fail"
  elif grep -qx 'ERROR:  42501' "${WORK}/key.err"; then key_denied=$((key_denied + 1)); else echo "OTHER: ${stmt}: $(head -1 "${WORK}/key.err")" >>"${WORK}/key.fail"; fi
  [ "$(leaks "${WORK}/key.out" "${WORK}/key.err")" = 0 ] || echo "LEAK: ${stmt}" >>"${WORK}/key.fail"
done
# the search_path detour: a schema without USAGE is silently skipped, so the unqualified name does not resolve (and nothing leaks)
if psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -v ON_ERROR_STOP=1 -c "SET search_path = actorproof_private, public" -c "SELECT secret FROM actor_proof_keys" >"${WORK}/key.out" 2>&1; then echo "NOT DENIED: search_path detour" >>"${WORK}/key.fail"; fi
[ "$(leaks "${WORK}/key.out")" = 0 ] || echo "LEAK: search_path detour" >>"${WORK}/key.fail"
for dargs in "-n actorproof_private" "-t actorproof_private.actor_proof_keys" "--schema-only" ""; do
  # shellcheck disable=SC2086
  if PGPASSFILE="$BPF" pg_dump -h 127.0.0.1 -p "$SRC_PORT" -U igaming_backup -Fp $dargs "$DB" >"${WORK}/key.dump" 2>"${WORK}/key.err"; then
    # --schema-only needs no lock on data, but must still be refused: pg_dump locks every table it dumps, and it cannot even see into the schema
    echo "pg_dump ${dargs:-<whole database>} SUCCEEDED as igaming_backup" >>"${WORK}/key.fail"
  elif ! grep -q 'permission denied for schema actorproof_private' "${WORK}/key.err"; then echo "pg_dump ${dargs:-<whole database>}: other error $(head -1 "${WORK}/key.err")" >>"${WORK}/key.fail"; fi
  [ "$(leaks "${WORK}/key.dump" "${WORK}/key.err")" = 0 ] || echo "LEAK: pg_dump ${dargs:-<whole database>}" >>"${WORK}/key.fail"
done
rm -f "${WORK}/key.dump" "${WORK}/key.out" "${WORK}/key.err"
KEY_PRIV="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "SELECT (has_schema_privilege(n.oid, 'USAGE') OR has_schema_privilege(n.oid, 'CREATE') OR has_table_privilege(c.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege(c.oid, 'SELECT,INSERT,UPDATE,REFERENCES'))::int FROM pg_namespace n JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = 'actor_proof_keys' WHERE n.nspname = 'actorproof_private'")"
KEY_FUNCS="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "SELECT count(*) FROM pg_proc p WHERE p.prosrc LIKE '%actorproof_private%' AND has_function_privilege(p.oid, 'EXECUTE')")"
KEY_FUNCS_ALL="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "SELECT count(*) FROM pg_proc p WHERE p.prosrc LIKE '%actorproof_private%'")"
SU_CTRL="$(pg_dump -h "$(cl_sock src)" -p "$SRC_PORT" -U postgres -Fp -n actorproof_private "$DB" | leaks)"
if [ "$key_denied" = "$key_total" ] && [ ! -s "${WORK}/key.fail" ] && [ "$KEY_PRIV" = 0 ] && [ "$KEY_FUNCS" = 0 ] && [ "$KEY_FUNCS_ALL" = 2 ] && [ "${SU_CTRL:-0}" -ge 2 ]; then
  res backup_role_cannot_read_actor_proof_key PASS "as igaming_backup: ${key_total}/${key_total} key probes denied with SQLSTATE 42501 (read secret, read kid, LOCK, CREATE in the schema, ALTER OWNER, GRANT USAGE to itself); the search_path detour does not resolve; pg_dump of the schema, the table, --schema-only and the whole database WITHOUT the exclusion all fail ('permission denied for schema actorproof_private') with 0 secret occurrences; no USAGE/CREATE/table/column privilege; of the ${KEY_FUNCS_ALL} functions that reach into the schema (actor_proof_require, actor_proof_key_active) the role may EXECUTE ${KEY_FUNCS}; positive control: the superuser's dump of the schema has ${SU_CTRL} leaking lines"
else res backup_role_cannot_read_actor_proof_key FAIL "denied=${key_denied}/${key_total} priv=${KEY_PRIV} key_funcs=${KEY_FUNCS}/${KEY_FUNCS_ALL} su_ctrl=${SU_CTRL} $(tr '\n' ';' <"${WORK}/key.fail")"; fi
ATTRS="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -F, -c "SELECT rolsuper::int, rolbypassrls::int, rolcreatedb::int, rolcreaterole::int, rolreplication::int, rolconnlimit, (SELECT count(*) FROM pg_auth_members WHERE member = r.oid), rolvaliduntil IS NOT NULL FROM pg_roles r WHERE rolname = 'igaming_backup'")"
if [ "$ATTRS" = "0,1,0,0,0,2,0,t" ]; then res backup_role_attributes PASS "NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 2, member of no role, VALID UNTIL set"; else res backup_role_attributes FAIL "attrs=${ATTRS}"; fi
( for i in 1 2; do psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "SELECT pg_sleep(6)" >/dev/null 2>&1 & done; sleep 2
  if psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "SELECT 1" >/dev/null 2>"${WORK}/connlimit.err"; then echo NOTLIMITED >"${WORK}/connlimit.res"; else grep -q 'too many connections' "${WORK}/connlimit.err" && echo LIMITED >"${WORK}/connlimit.res" || echo OTHER >"${WORK}/connlimit.res"; fi; wait ) || true
if [ "$(cat "${WORK}/connlimit.res")" = LIMITED ]; then res backup_role_connection_limit_enforced PASS "a third concurrent session is refused (CONNECTION LIMIT 2)"; else res backup_role_connection_limit_enforced FAIL "$(cat "${WORK}/connlimit.res")"; fi
# audit evidence: connections + statements logged for the role (log_connections/log_disconnections cluster-wide, log_statement role-level)
AUD_CONN="$(grep -c 'connection authorized: user=igaming_backup' "${DRILL_ROOT}/src/server.log" || true)"
AUD_STMT="$(grep -c 'igaming_backup@.*LOG:  statement:' "${DRILL_ROOT}/src/server.log" || true)"
AUD_DISC="$(grep -c 'disconnection: .*user=igaming_backup' "${DRILL_ROOT}/src/server.log" || true)"
if [ "$AUD_CONN" -gt 0 ] && [ "$AUD_STMT" -gt 0 ] && [ "$AUD_DISC" -gt 0 ]; then res backup_role_activity_is_audited PASS "server log holds ${AUD_CONN} connection, ${AUD_DISC} disconnection and ${AUD_STMT} statement lines for igaming_backup (log_connections, log_disconnections, ALTER ROLE ... SET log_statement; on RDS pgaudit)"; else res backup_role_activity_is_audited FAIL "conn=${AUD_CONN} disc=${AUD_DISC} stmt=${AUD_STMT}"; fi

# M1: what a STOLEN backup credential could abuse is narrowed by harden-backup-surface.sql (the platform roles keep what they use)
m1_denied=0; m1_total=0
for stmt in "SELECT pg_advisory_lock(42)" "SELECT pg_try_advisory_lock(42)" "SELECT pg_advisory_xact_lock(42)" "SELECT pg_logical_emit_message(false, 'x', 'y')" \
            "SELECT lo_create(0)" "SELECT pg_notify('c', 'p')"; do
  m1_total=$((m1_total + 1))
  if psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "$stmt" >/dev/null 2>"${WORK}/m1.err"; then echo "NOT DENIED: ${stmt}" >>"${WORK}/m1.fail"; elif grep -q 'permission denied for function' "${WORK}/m1.err"; then m1_denied=$((m1_denied + 1)); else echo "OTHER ERROR: ${stmt}: $(head -1 "${WORK}/m1.err")" >>"${WORK}/m1.fail"; fi
done
for d in postgres template1; do
  m1_total=$((m1_total + 1))
  if psql_as igaming_backup "$BPF" "$SRC_PORT" "$d" -c "SELECT 1" >/dev/null 2>"${WORK}/m1.err"; then echo "CONNECT NOT DENIED: ${d}" >>"${WORK}/m1.fail"; elif grep -q 'permission denied for database' "${WORK}/m1.err"; then m1_denied=$((m1_denied + 1)); else echo "OTHER: connect ${d}: $(head -1 "${WORK}/m1.err")" >>"${WORK}/m1.fail"; fi
done
if [ "$m1_denied" = "$m1_total" ]; then res backup_role_abuse_primitives_denied PASS "${m1_total}/${m1_total} denied for igaming_backup: session advisory locks (migration/withdrawal/reconciliation stall), pg_logical_emit_message (unbounded WAL), large objects, NOTIFY, CONNECT to postgres/template1"; else res backup_role_abuse_primitives_denied FAIL "$(tr '\n' ';' <"${WORK}/m1.fail")"; fi
if psql_as igaming_runtime "$OPF" "$SRC_PORT" "$DB" -c "SELECT pg_try_advisory_lock(42), pg_advisory_unlock(42)" >/dev/null 2>&1 && psql_as igaming "$OPF" "$SRC_PORT" "$DB" -c "SELECT pg_try_advisory_lock(43), pg_advisory_unlock(43)" >/dev/null 2>&1; then res hardening_keeps_platform_roles_working PASS "igaming and igaming_runtime still take advisory locks (reconciliation, status gates); the full seed ran after the hardening"; else res hardening_keeps_platform_roles_working FAIL "platform role lost an advisory-lock privilege"; fi

# M2: the credential minting path never puts the password into statement text. Superuser log_statement=all for the minting session
# (the worst case for a log leak); a control shows that this logging WOULD capture a naive ALTER ROLE ... PASSWORD.
LOGBYTES="$(stat -c %s "${DRILL_ROOT}/src/server.log")"
PGOPTIONS='-c log_statement=all' prov_backup_role src "$SRC_PORT" "$FUTURE"
MINT_PW="$BK_LAST_BACKUP_PW"; BPF="${SECRETS}/pgpass.backup"
tail -c +"$((LOGBYTES + 1))" "${DRILL_ROOT}/src/server.log" >"${WORK}/mint.log"
CTRLPW="$(cl_random_secret)"
PGOPTIONS='-c log_statement=all' IGPW="$CTRLPW" cl_super src "$SRC_PORT" -d "$DB" <<'SQL' >/dev/null
\getenv pw IGPW
CREATE ROLE zz_logctl LOGIN PASSWORD :'pw';
DROP ROLE zz_logctl;
SQL
tail -c +"$((LOGBYTES + 1))" "${DRILL_ROOT}/src/server.log" >"${WORK}/mint-ctl.log"
if grep -q 'igaming.backup_verifier' "${WORK}/mint.log" && ! grep -qF "$MINT_PW" "${WORK}/mint.log" && grep -qF "$CTRLPW" "${WORK}/mint-ctl.log"; then
  res backup_credential_never_in_server_log PASS "log_statement=all during minting: the statements are logged ($(grep -c 'LOG:  statement' "${WORK}/mint.log") lines) but the password is NOT in the server log; control: a naive CREATE ROLE ... PASSWORD :'pw' DOES leak its password into the same log"
else res backup_credential_never_in_server_log FAIL "verifier_logged=$(grep -c 'igaming.backup_verifier' "${WORK}/mint.log") leaked=$(grep -cF "$MINT_PW" "${WORK}/mint.log") control_leaked=$(grep -cF "$CTRLPW" "${WORK}/mint-ctl.log")"; fi
rm -f "${WORK}/mint.log" "${WORK}/mint-ctl.log"
# the password itself must not be accepted where a verifier is expected
if IGAMING_BACKUP_PASSWORD_VERIFIER="plain-password" cl_super src "$SRC_PORT" -d "$DB" -f "${SCRIPT_DIR}/init-backup-role.sql" >/dev/null 2>"${WORK}/verif.err"; then res init_backup_role_rejects_cleartext_password FAIL "a clear-text value was accepted"; else res init_backup_role_rejects_cleartext_password PASS "refused: $(grep -m1 -o 'must be a SCRAM-SHA-256 verifier[^"]*' "${WORK}/verif.err" | cut -c1-90)"; fi
if IGAMING_BACKUP_PASSWORD_VERIFIER="$(printf x | "$SCRAMHASH_BIN")" BACKUP_ROLE_VALID_UNTIL=infinity cl_super src "$SRC_PORT" -d "$DB" -f "${SCRIPT_DIR}/init-backup-role.sql" >/dev/null 2>"${WORK}/verif.err"; then res init_backup_role_rejects_unbounded_validity FAIL "infinity accepted"; else res init_backup_role_rejects_unbounded_validity PASS "VALID UNTIL 'infinity' refused (credential validity is bounded to 48 h)"; fi
# the refused statements above must not have disturbed the minted credential
psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c 'SELECT 1' >/dev/null 2>&1 || bk_die "the backup credential stopped working after the refused re-mint attempts"

# ================================================================= phase 3: logical backup -> restore -> verify
section "3 logical backup (backup role) -> restore into a new database -> verify"
export BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT="$SRC_PORT" BACKUP_PGDATABASE="$DB" BACKUP_PGUSER=igaming_backup BACKUP_PGPASSFILE="$BPF"
# enable continuous WAL archiving now (restart): the physical-recovery window starts here
cl_stop src
cat >>"${DRILL_ROOT}/src/data/postgresql.conf" <<EOF
archive_mode = on
archive_command = 'test ! -f ${DRILL_ROOT}/src/wal_archive/%f && cp %p ${DRILL_ROOT}/src/wal_archive/%f'
EOF
cl_start src "$SRC_PORT"
bk_check_free 450 "backup"
# the unencrypted-by-default refusal
if OUTP="$("${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" 2>&1)"; then res backup_refuses_without_encryption_policy FAIL "unencrypted backup was produced"; else case "$OUTP" in *"no encryption recipient configured"*) res backup_refuses_without_encryption_policy PASS "refused: $(printf '%s' "$OUTP" | cut -c1-110)" ;; *) res backup_refuses_without_encryption_policy FAIL "$OUTP" ;; esac; fi
export BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only
BOUT="$("${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label plain 2>&1)" || { echo "$BOUT" >&2; bk_die "plain backup failed"; }; echo "$BOUT" | grep -E '^(TIMING|SIZE)' | tee -a "$OUT"
PLAIN_DIR="$(printf '%s\n' "$BOUT" | sed -n 's/^RUN_DIR=//p')"
[ -d "$PLAIN_DIR" ] || { echo "$BOUT" >&2; bk_die "backup failed"; }
SETLINE="$(printf '%s\n' "$BOUT" | sed -n 's/^SET_SHA256=//p')"
if [ "$SETLINE" = "$(sha256sum <"${PLAIN_DIR}/SHA256SUMS" | cut -d' ' -f1)" ]; then res backup_prints_set_sha256_for_out_of_band_record PASS "SET_SHA256=${SETLINE} printed after signing; it is the value restore.sh requires as RESTORE_EXPECT_SET_SHA256"; else res backup_prints_set_sha256_for_out_of_band_record FAIL "printed '${SETLINE}'"; fi
meas "SIZE|backup_directory_bytes|$(du -sb "$PLAIN_DIR" | cut -f1)"
res logical_backup_written PASS "$(basename "$PLAIN_DIR"): $(ls "$PLAIN_DIR" | tr '\n' ' ')"
# the baseline (and therefore verify.sh) must cover the tables, FORCE RLS, triggers and SECURITY DEFINER functions of the latest migrations (AML screening 0154/0155, alert retention 0157)
BTAB="$(grep -c '^TABLE|' "${PLAIN_DIR}/baseline.txt")"; BHEAD="$(grep '^META|migration_head|' "${PLAIN_DIR}/baseline.txt" | cut -d'|' -f3)"
COV_MISSING=""
for t in aml_screening_policies aml_screening_results alert_retention_runs alerts alert_deliveries; do
  grep -q "^TABLE|${t}|" "${PLAIN_DIR}/baseline.txt" || COV_MISSING="${COV_MISSING} TABLE:${t}"
  grep -q "^RLS|${t}|enabled=1 forced=1" "${PLAIN_DIR}/baseline.txt" || COV_MISSING="${COV_MISSING} FORCE-RLS:${t}"
done
for f in aml_screening_policy_set aml_screening_record aml_screening_list; do grep -q "^FUNCACL|${f}(" "${PLAIN_DIR}/baseline.txt" || COV_MISSING="${COV_MISSING} FUNCACL:${f}"; done
grep -q '^TRG|aml_screening_results|' "${PLAIN_DIR}/baseline.txt" || COV_MISSING="${COV_MISSING} TRG:aml_screening_results"
if [ -z "$COV_MISSING" ]; then res baseline_covers_latest_migrations PASS "migration head ${BHEAD}; ${BTAB} tables hashed, incl. aml_screening_policies/results, alert_retention_runs, alerts, alert_deliveries with FORCE RLS, the AML SECURITY DEFINER functions' ACLs and their append-only triggers"; else res baseline_covers_latest_migrations FAIL "missing from the baseline:${COV_MISSING}"; fi
if grep -q '"encryption": "none"' "${PLAIN_DIR}/manifest.json" && grep -q '"environment_anchor": "development"' "${PLAIN_DIR}/manifest.json" && [ -s "${PLAIN_DIR}/SHA256SUMS.sig" ] && grep -q ' manifest.json$' "${PLAIN_DIR}/SHA256SUMS"; then res plain_backup_manifest_is_truthful_and_signed PASS "manifest says encryption=none (synthetic override), environment_anchor=development; manifest.json is covered by the signed SHA256SUMS"; else res plain_backup_manifest_is_truthful_and_signed FAIL "$(cat "${PLAIN_DIR}/manifest.json" | head -12 | tr '\n' ' ')"; fi
# ---- owner decision 5 (ADR 0122 section 10) / F1: the dump carries NOTHING of the schema actorproof_private (no definition, no rows) and no actor_proof_key*
# data entry in any schema; the verifier functions are there; no secret byte is anywhere in the set, manifest or logs
APK_TOC="$(pg_restore --list "${PLAIN_DIR}/db.dump")"
APK_SCHEMA_ENTRIES="$(grep -c -E '(^| )actorproof_private( |$)' <<<"$APK_TOC" || true)"
APK_DATA_ENTRY="$(grep -c -E ' TABLE DATA [^ ]+ actor_proof_key[^ ]* ' <<<"$APK_TOC" || true)"
APK_VERIFIER=0; grep -q ' FUNCTION public actor_proof_require(' <<<"$APK_TOC" && grep -q ' FUNCTION public actor_proof_key_active(' <<<"$APK_TOC" && APK_VERIFIER=1
LEAK_DUMP="$(pg_restore -f - "${PLAIN_DIR}/db.dump" | leaks)"                 # the dump as SQL text (the custom format is compressed: grep the decompressed content)
LEAK_RAW="$(leaks "${PLAIN_DIR}/db.dump")"                                   # and the raw bytes
LEAK_SET="$(leaks "${PLAIN_DIR}/globals.sql" "${PLAIN_DIR}/baseline.txt" "${PLAIN_DIR}/manifest.json" "${PLAIN_DIR}/SHA256SUMS")"
LEAK_LOG="$(printf '%s\n' "$BOUT" | leaks)"                                  # the backup run's own output
LEAK_SRVLOG="$(leaks "${DRILL_ROOT}/src/server.log")"                        # the server log (incl. the audited statements of igaming_backup)
# positive control: the SAME hunt finds both secrets in the superuser's dump of the private schema (so a zero above is a real zero, not a blind grep)
CTRL_LEAKS="$(pg_dump -h "$(cl_sock src)" -p "$SRC_PORT" -U postgres -Fp -n actorproof_private "$DB" | leaks)"
BL_AP_LINE="$(grep '^TABLE|actor_proof_keys|' "${PLAIN_DIR}/baseline.txt" || true)"
BL_AP_KIDS="$(grep '^META|actor_proof_kids|' "${PLAIN_DIR}/baseline.txt" | cut -d'|' -f3- || true)"
BL_AP_INV="$(grep '^INV|actor_proof_key_reachable_by_non_owner|' "${PLAIN_DIR}/baseline.txt" || true)"
BL_AP_FP="$(grep '^FP|actorproof_private_md5|' "${PLAIN_DIR}/baseline.txt" || true)"
if [ "$APK_SCHEMA_ENTRIES" = 0 ] && [ "$APK_DATA_ENTRY" = 0 ] && [ "$APK_VERIFIER" = 1 ] && grep -q '"actor_proof_keys_data": "excluded"' "${PLAIN_DIR}/manifest.json" \
   && [ "$LEAK_DUMP" = 0 ] && [ "$LEAK_RAW" = 0 ] && [ "$LEAK_SET" = 0 ] && [ "$LEAK_LOG" = 0 ] && [ "$LEAK_SRVLOG" = 0 ] && [ "${CTRL_LEAKS:-0}" -ge 2 ] \
   && [ -z "$BL_AP_LINE" ] && [ -z "$BL_AP_KIDS" ] \
   && [ "$BL_AP_INV" = "INV|actor_proof_key_reachable_by_non_owner|0" ] && [ -n "$BL_AP_FP" ] && [ "${BL_AP_FP##*|count=}" != 0 ]; then
  res logical_dump_excludes_actor_proof_secret PASS "pg_restore --list: 0 entries of the schema actorproof_private, 0 actor_proof_key* data entries, the verifier and the key probe present; manifest actor_proof_keys_data=excluded; the planted canary and the seed's platform key (ASCII/hex/base64) occur 0 times in the dump (SQL text ${LEAK_DUMP}, raw ${LEAK_RAW}), 0 in globals/baseline/manifest/SHA256SUMS (${LEAK_SET}), 0 in the backup run output (${LEAK_LOG}), 0 in the server log (${LEAK_SRVLOG}); positive control: the superuser's dump of the schema has ${CTRL_LEAKS} leaking lines; the baseline (taken by the backup role) holds no key row line and no key id at all, only the private schema's structure fingerprint (${BL_AP_FP#FP|}) and 0 non-owner paths to it"
else res logical_dump_excludes_actor_proof_secret FAIL "schema_entries=${APK_SCHEMA_ENTRIES} data_entries=${APK_DATA_ENTRY} verifier=${APK_VERIFIER} leaks dump=${LEAK_DUMP} raw=${LEAK_RAW} set=${LEAK_SET} log=${LEAK_LOG} srvlog=${LEAK_SRVLOG} control=${CTRL_LEAKS} baseline='${BL_AP_LINE}' kids='${BL_AP_KIDS}' inv='${BL_AP_INV}' fp='${BL_AP_FP}' manifest=$(grep actor_proof "${PLAIN_DIR}/manifest.json")"; fi
# encryption with an EPHEMERAL age key generated inside the drill root (no key material in the repository)
age-keygen -o "${SECRETS}/age.key" >/dev/null 2>&1; chmod 600 "${SECRETS}/age.key"
AGE_PUB="$(age-keygen -y "${SECRETS}/age.key")"
EOUT="$(env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label age 2>&1)" || { echo "$EOUT" >&2; bk_die "age backup failed"; }; echo "$EOUT" | grep -E '^TIMING\|backup_(total|encrypt)' | sed 's/backup_/backup_encrypted_/' | tee -a "$OUT"
ENC_DIR="$(printf '%s\n' "$EOUT" | sed -n 's/^RUN_DIR=//p')"
[ -d "$ENC_DIR" ] && ls "$ENC_DIR"/db.dump.age >/dev/null 2>&1 && ! ls "$ENC_DIR"/db.dump >/dev/null 2>&1 && res encryption_hook_age PASS "artifacts age-encrypted ($(ls "$ENC_DIR" | tr '\n' ' ')); no plaintext dump left" || res encryption_hook_age FAIL "$EOUT"

# the encrypted set holds the same exclusion: decrypt with the ephemeral identity (inside the private temp root), hunt the secrets in the plaintext, remove it
AGE_DEC="$(mktemp -d "${BACKUP_TMPDIR}/ap-dec.XXXXXX")"
age -d -i "${SECRETS}/age.key" -o "${AGE_DEC}/db.dump" "${ENC_DIR}/db.dump.age" && age -d -i "${SECRETS}/age.key" -o "${AGE_DEC}/globals.sql" "${ENC_DIR}/globals.sql.age" \
  && age -d -i "${SECRETS}/age.key" -o "${AGE_DEC}/baseline.txt" "${ENC_DIR}/baseline.txt.age"
ENC_LEAK="$(( $(pg_restore -f - "${AGE_DEC}/db.dump" | leaks) + $(leaks "${AGE_DEC}/db.dump" "${AGE_DEC}/globals.sql" "${AGE_DEC}/baseline.txt" "${ENC_DIR}/manifest.json" "${ENC_DIR}/SHA256SUMS") + $(printf '%s\n' "$EOUT" | leaks) ))"
ENC_TOC_DATA="$(pg_restore --list "${AGE_DEC}/db.dump" | grep -c -E ' TABLE DATA [^ ]+ actor_proof_key[^ ]* |(^| )actorproof_private( |$)' || true)"
rm -rf "$AGE_DEC"
if [ "$ENC_LEAK" = 0 ] && [ "$ENC_TOC_DATA" = 0 ] && grep -q '"actor_proof_keys_data": "excluded"' "${ENC_DIR}/manifest.json"; then
  res encrypted_dump_excludes_actor_proof_secret PASS "the age-encrypted set, decrypted: no actor_proof_key* data entry and no entry of the schema actorproof_private, and 0 occurrences of any planted/seeded secret form in db.dump (SQL text and raw), globals, baseline, manifest, SHA256SUMS or the backup output; the decrypted copy was removed"
else res encrypted_dump_excludes_actor_proof_secret FAIL "leaks=${ENC_LEAK} data_entries=${ENC_TOC_DATA}"; fi

# ---- H1: encryption fails closed, plaintext never at rest -----------------------------------------------------------
H1OUT="${DRILL_ROOT}/h1out"; mkdir -m 700 "$H1OUT"
no_tmp_left() { [ -z "$(find "$BACKUP_TMPDIR" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; }
find_plain() { find "$1" \( -name db.dump -o -name globals.sql -o -name baseline.txt -o -name '*.partial' \) 2>/dev/null; }
if [ -z "$(find_plain "$ENC_DIR")" ] && [ "$(head -c 21 "${ENC_DIR}/db.dump.age")" = "age-encryption.org/v1" ] && [ "$(head -c 21 "${ENC_DIR}/globals.sql.age")" = "age-encryption.org/v1" ] \
   && ! grep -aq PGDMP "${ENC_DIR}/db.dump.age" && ! grep -aq 'CREATE ROLE' "${ENC_DIR}/globals.sql.age" && ! grep -aq 'TABLE|' "${ENC_DIR}/baseline.txt.age" \
   && grep -q '"encryption": "age"' "${ENC_DIR}/manifest.json"; then
  res encrypted_backup_is_really_encrypted PASS "age magic on db.dump/globals.sql/baseline.txt, no PGDMP/CREATE ROLE/TABLE| plaintext markers in the files, no plaintext artefact or .partial in the run directory, manifest encryption=age (derived from the files, not from configuration)"
else res encrypted_backup_is_really_encrypted FAIL "plain=$(find_plain "$ENC_DIR" | tr '\n' ' ') $(grep encryption "${ENC_DIR}/manifest.json")"; fi
# no pg_dump archive (PGDMP magic) may exist anywhere except the deliberately plaintext synthetic set: not in the repo working directory (a literal
# file named "-" is what `pg_dump --file=-` would create), not in the controlled temp root, work dir or other backup dirs
pgdmp_files() {
  local f
  while IFS= read -r f; do
    case "$f" in "${PLAIN_DIR}"/*) continue ;; esac
    [ "$(head -c 5 "$f" 2>/dev/null)" = PGDMP ] && echo "$f"
  done < <(find "$REPO_ROOT" -maxdepth 1 -type f -size +1k; find "$BKUP" "$WORK" "$BACKUP_TMPDIR" "$(pwd)" -maxdepth 3 -type f -size +1k 2>/dev/null)
  return 0
}
PGD="$(pgdmp_files | sort -u | tr '\n' ' ')"
if [ -z "$PGD" ]; then res no_plaintext_archive_outside_the_synthetic_set PASS "no PGDMP archive in the repo working directory, backup root, work dir or controlled temp root other than the deliberately plaintext synthetic set"; else res no_plaintext_archive_outside_the_synthetic_set FAIL "plaintext archive(s): ${PGD}"; fi
expect_refused() { # name message-regex command...   : the command must fail with the message and leave NOTHING behind
  local name="$1" rx="$2" out rc; shift 2
  out="$("$@" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qE "$rx" && [ -z "$(ls -A "$H1OUT")" ] && no_tmp_left; then
    res "$name" PASS "exit ${rc}: $(printf '%s' "$out" | head -1 | cut -c1-160); no run directory, no plaintext, no temporary files left"
  else res "$name" FAIL "rc=${rc} out=${out:0:300} left=$(ls -A "$H1OUT" | tr '\n' ' ')"; fi
}
LASTC="${AGE_PUB: -1}"; if [ "$LASTC" = q ]; then SUBC=p; else SUBC=q; fi
BADR="${AGE_PUB%?}${SUBC}"   # a well-formed-looking recipient with a corrupted checksum (a typo)
expect_refused backup_mistyped_age_recipient_fails_closed 'cannot encrypt to BACKUP_ENCRYPT_AGE_RECIPIENT.*nothing was written' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="$BADR" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label bad
expect_refused backup_malformed_age_recipient_fails_closed 'not a well-formed age recipient' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="age1notarecipient" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label bad
# negative controls for the exclusion (owner decision 5, F1): (a) with the exclusion removed (drill-only fault) the backup ROLE cannot dump the private schema at
# all (no USAGE); (b) a backup role that WAS mis-granted the key (drill-only GRANT by the throwaway superuser) is refused by backup.sh's preflight; (c) with the
# mis-grant AND the fault (which also skips that preflight) the dump stream does carry the key, and the TOC check on the encrypted stream REFUSES the backup and
# keeps nothing. The mis-grant is removed right after.
expect_refused backup_role_cannot_dump_actor_proof_schema_even_without_exclusion 'permission denied for schema actorproof_private' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_TEST_FAULT=include_actor_proof_keys BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label apk
cl_super src "$SRC_PORT" -d "$DB" -c "GRANT USAGE ON SCHEMA actorproof_private TO igaming_backup" -c "GRANT SELECT ON actorproof_private.actor_proof_keys TO igaming_backup" >/dev/null
expect_refused backup_refused_when_backup_role_can_read_actor_proof_key 'can read the actor-proof signing key' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label apkgrant
expect_refused backup_with_actor_proof_rows_in_stream_is_refused 'carries the DATA of an actor_proof_keys table|carries entries of the schema actorproof_private' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_TEST_FAULT=include_actor_proof_keys BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label apkfault
cl_super src "$SRC_PORT" -d "$DB" -c "REVOKE SELECT ON actorproof_private.actor_proof_keys FROM igaming_backup" -c "REVOKE USAGE ON SCHEMA actorproof_private FROM igaming_backup" >/dev/null
[ "$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$DB" -c "SELECT (has_schema_privilege(n.oid, 'USAGE') OR has_table_privilege(c.oid, 'SELECT'))::int FROM pg_namespace n JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = 'actor_proof_keys' WHERE n.nspname = 'actorproof_private'")" = 0 ] \
  || bk_die "the drill's deliberate mis-grant on actorproof_private could not be removed"
expect_refused backup_unknown_gpg_key_fails_closed 'is not in the keyring|must be a FULL 40-hex' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_GPG_RECIPIENT="0000000000000000000000000000000000000000" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label bad
expect_refused backup_short_gpg_id_refused 'must be a FULL 40-hex' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_GPG_RECIPIENT="DEADBEEF" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label bad
expect_refused backup_unencrypted_override_refused_for_remote_target 'no encryption recipient configured' \
  env BACKUP_PGHOST=db.example.invalid BACKUP_ALLOW_REMOTE_TARGET=db.example.invalid BACKUP_PGSSLROOTCERT="${SECRETS}/sign.pub" BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label remote
expect_refused backup_unencrypted_override_needs_exact_value 'no encryption recipient configured' \
  env BACKUP_ALLOW_UNENCRYPTED=yes "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label yes
# fault shims: an `age` that works for the probe and then dies / stalls mid-stream
REAL_AGE="$(command -v age)"
mkdir -p "${SECRETS}/shim-die" "${SECRETS}/shim-slow"
cat >"${SECRETS}/shim-die/age" <<EOF
#!/usr/bin/env bash
# DRILL fault shim: the first call (the recipient probe) is the real age; the second (the dump stream) consumes some input and is SIGKILLed
st="${SECRETS}/shim-die/state"
if [ ! -e "\$st" ]; then : >"\$st"; exec "${REAL_AGE}" "\$@"; fi
head -c 65536 >/dev/null; kill -9 \$\$
EOF
cat >"${SECRETS}/shim-slow/age" <<EOF
#!/usr/bin/env bash
# DRILL fault shim: probe is real; the dump-stream encryptor stalls for 120 s before doing anything (the dump is "in flight")
st="${SECRETS}/shim-slow/state"
if [ ! -e "\$st" ]; then : >"\$st"; exec "${REAL_AGE}" "\$@"; fi
sleep 120 & wait \$!
exec "${REAL_AGE}" "\$@"
EOF
chmod 755 "${SECRETS}/shim-die/age" "${SECRETS}/shim-slow/age"
rm -f "${SECRETS}/shim-die/state"
expect_refused backup_encryptor_killed_midstream_fails_closed 'pg_dump or the encryptor failed|failed' \
  env -u BACKUP_ALLOW_UNENCRYPTED PATH="${SECRETS}/shim-die:$PATH" BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label killedenc

# ---- H2: abnormal exit, long dumps, stale temporaries ---------------------------------------------------------------
agent_sessions() { cl_super src "$SRC_PORT" -d postgres -c "SELECT count(*) FROM pg_stat_activity WHERE usename = 'igaming_backup'"; }
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming_backup SET idle_in_transaction_session_timeout = '3s'" >/dev/null
OUTP="$(BACKUP_KEEPALIVE_SECONDS=1 BACKUP_TEST_FAULT=slow_dump BACKUP_TEST_SLOW_SECONDS=9 env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label keepalive 2>&1)" && RC=0 || RC=$?
KA_DIR="$(printf '%s\n' "$OUTP" | sed -n 's/^RUN_DIR=//p')"
if [ "$RC" -eq 0 ] && [ -d "$KA_DIR" ] && [ -s "${KA_DIR}/SHA256SUMS.sig" ]; then res backup_snapshot_holder_survives_idle_timeout_via_keepalive PASS "role idle_in_transaction_session_timeout=3s, the dump was delayed 9 s (3x the timeout): the keepalive kept the exported snapshot alive and the backup completed and was signed"; else res backup_snapshot_holder_survives_idle_timeout_via_keepalive FAIL "rc=${RC} $OUTP"; fi
rm -rf "$KA_DIR"
OUTP="$(BACKUP_KEEPALIVE_SECONDS=3600 BACKUP_TEST_FAULT=slow_dump BACKUP_TEST_SLOW_SECONDS=9 env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label nokeepalive 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ -z "$(ls -A "$H1OUT")" ] && no_tmp_left; then res backup_without_keepalive_holder_death_fails_loudly PASS "control: with the keepalive disabled the server's idle timeout killed the snapshot holder and the backup FAILED (exit ${RC}: $(printf '%s' "$OUTP" | grep -m1 'backup:' | cut -c1-150)); nothing kept"; else res backup_without_keepalive_holder_death_fails_loudly FAIL "rc=${RC} $OUTP"; fi
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming_backup SET idle_in_transaction_session_timeout = '30min'" >/dev/null
expect_refused backup_killed_snapshot_holder_fails_loudly 'snapshot|pg_dump or the encryptor failed' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_TEST_FAULT=kill_holder BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label killholder
sig_test() { # SIGNAL expected-exit : a signal while the dump is in flight (encryptor stalled)
  local sig="$1" want="$2" bp rc part
  rm -f "${SECRETS}/shim-slow/state"
  set -m   # job control: a background job started WITHOUT it has SIGINT ignored (not a real-world case: Ctrl-C reaches the foreground job)
  env -u BACKUP_ALLOW_UNENCRYPTED PATH="${SECRETS}/shim-slow:$PATH" BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label "sig${sig}" >"${WORK}/sig.out" 2>&1 &
  bp=$!
  set +m
  for _ in $(seq 1 150); do [ -n "$(find "$H1OUT" -name '*.partial' 2>/dev/null)" ] && break; sleep 0.2; done
  part="$(find "$H1OUT" -name '*.partial' | wc -l)"
  kill -"$sig" "$bp"; wait "$bp" && rc=0 || rc=$?
  sleep 2
  if [ "$rc" -eq "$want" ] && [ "$part" -ge 1 ] && [ -z "$(ls -A "$H1OUT")" ] && no_tmp_left && [ "$(agent_sessions)" = 0 ] && [ "$(ps -eo args | grep -c '^sleep 120$' || true)" = 0 ]; then
    res "backup_sig${sig}_mid_dump_cleans_up" PASS "SIG${sig} sent while a .partial ciphertext existed (dump in flight): exit ${rc}; run directory and temp dir removed, no plaintext, snapshot-holder session gone, encryptor child terminated"
  else res "backup_sig${sig}_mid_dump_cleans_up" FAIL "rc=${rc} (want ${want}) partial_seen=${part} left=$(ls -A "$H1OUT" | tr '\n' ' ') tmp=$(ls -A "$BACKUP_TMPDIR" | tr '\n' ' ') sessions=$(agent_sessions) $(tail -2 "${WORK}/sig.out" | tr '\n' ' ')"; fi
}
sig_test TERM 143
sig_test INT 130
# SIGPIPE: the consumer of stdout goes away; the signal is ignored and handled as an error with full cleanup
set +e; env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label pipe 2>"${WORK}/pipe.err" | head -c 0; RC=${PIPESTATUS[0]}; set -e
if [ "$RC" -ne 0 ] && [ "$RC" -ne 141 ] && [ -z "$(ls -A "$H1OUT")" ] && no_tmp_left; then res backup_sigpipe_closed_stdout_cleans_up PASS "stdout closed by the consumer: exit ${RC} (not a silent SIGPIPE death), no output directory kept, no temp files left"; else res backup_sigpipe_closed_stdout_cleans_up FAIL "rc=${RC} left=$(ls -A "$H1OUT" | tr '\n' ' ')"; fi
# startup sweep of the leavings of a SIGKILLed run (stale temp dir; orphaned incomplete run directory)
mkdir -m 700 "${BACKUP_TMPDIR}/bk-run.STALE001"; echo 2147483 >"${BACKUP_TMPDIR}/bk-run.STALE001/.pid"; echo secret >"${BACKUP_TMPDIR}/bk-run.STALE001/baseline.txt"
mkdir -m 700 "${H1OUT}/20200101T000000Z-orphan"; printf "%s:2147483" "$(hostname)" >"${H1OUT}/20200101T000000Z-orphan/.incomplete"; echo x >"${H1OUT}/20200101T000000Z-orphan/db.dump.partial"
# a symlink named like a run directory, whose TARGET holds an .incomplete marker, must NOT have its contents deleted (SR12R-6); neither must a run recorded by another host
mkdir -m 700 "${DRILL_ROOT}/symtarget"; printf "%s:2147483" "$(hostname)" >"${DRILL_ROOT}/symtarget/.incomplete"; echo keep >"${DRILL_ROOT}/symtarget/keepme"
ln -s "${DRILL_ROOT}/symtarget" "${H1OUT}/20200101T000001Z-link"
mkdir -m 700 "${H1OUT}/20200101T000002Z-otherhost"; printf "otherhost.invalid:2147483" >"${H1OUT}/20200101T000002Z-otherhost/.incomplete"
env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_ENCRYPT_AGE_RECIPIENT="$BADR" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label sweep >/dev/null 2>&1 || true
if [ -f "${DRILL_ROOT}/symtarget/keepme" ] && [ -d "${H1OUT}/20200101T000002Z-otherhost" ] && [ -L "${H1OUT}/20200101T000001Z-link" ]; then res backup_sweep_ignores_symlinks_and_other_hosts_runs PASS "a symlinked run name and a run recorded by another host were left untouched (only real same-host orphans are removed)"; else res backup_sweep_ignores_symlinks_and_other_hosts_runs FAIL "symlink target contents or foreign run were deleted"; fi
rm -rf "${H1OUT}/20200101T000001Z-link" "${H1OUT}/20200101T000002Z-otherhost" "${DRILL_ROOT}/symtarget"
if [ ! -e "${BACKUP_TMPDIR}/bk-run.STALE001" ] && [ ! -e "${H1OUT}/20200101T000000Z-orphan" ] && no_tmp_left; then res backup_startup_sweeps_stale_temp_and_orphan_runs PASS "a stale temp directory and an orphaned incomplete run directory of a dead pid were removed at the next start"; else res backup_startup_sweeps_stale_temp_and_orphan_runs FAIL "tmp=$(ls -A "$BACKUP_TMPDIR" | tr '\n' ' ') out=$(ls -A "$H1OUT" | tr '\n' ' ')"; fi
rm -rf "${H1OUT:?}"/* 2>/dev/null || true
TMPMODE="$(stat -c %a "$BACKUP_TMPDIR")"
if [ "$TMPMODE" = 700 ]; then res backup_temp_root_is_private PASS "controlled temp root mode ${TMPMODE}; per-run directories are mktemp -d (0700) with unpredictable names"; else res backup_temp_root_is_private FAIL "mode ${TMPMODE}"; fi


# ---- SR12R-4: a remote target must use sslmode=verify-full with a root certificate -------------------------------------------------------------------------
expect_refused backup_remote_target_requires_root_cert 'BACKUP_PGSSLROOTCERT' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_PGHOST=db.example.invalid BACKUP_ALLOW_REMOTE_TARGET=db.example.invalid BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label ssl
expect_refused backup_remote_target_refuses_weaker_sslmode 'requires sslmode=verify-full' \
  env -u BACKUP_ALLOW_UNENCRYPTED BACKUP_PGHOST=db.example.invalid BACKUP_ALLOW_REMOTE_TARGET=db.example.invalid BACKUP_PGSSLMODE=require BACKUP_PGSSLROOTCERT="${SECRETS}/sign.pub" BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label ssl2
expect_refused backup_remote_target_requires_private_scratch_dir 'BACKUP_TMPDIR' \
  env -u BACKUP_ALLOW_UNENCRYPTED -u BACKUP_TMPDIR BACKUP_PGHOST=db.example.invalid BACKUP_ALLOW_REMOTE_TARGET=db.example.invalid BACKUP_PGSSLROOTCERT="${SECRETS}/sign.pub" BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label tmp

# ---- SR12R-9: IAM-token mode - a fresh token (here: the role's password standing in for a 15-minute IAM token) is written to a private passfile before EVERY connection ----
printf '%s\n' "$(pw_of "$BPF" "$SRC_PORT" igaming_backup)" >"${SECRETS}/token"; chmod 600 "${SECRETS}/token"
OUTP="$(env -u BACKUP_ALLOW_UNENCRYPTED -u BACKUP_PGPASSFILE BACKUP_IAM_TOKEN_COMMAND="cat ${SECRETS}/token" BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label iam 2>&1)" && RC=0 || RC=$?
IAM_DIR="$(printf '%s\n' "$OUTP" | sed -n 's/^RUN_DIR=//p')"
if [ "$RC" -eq 0 ] && [ -s "${IAM_DIR}/SHA256SUMS.sig" ] && [ -s "${IAM_DIR}/globals.sql.age" ] && no_tmp_left; then res backup_iam_token_mode_refreshes_per_connection PASS "no password passfile configured: BACKUP_IAM_TOKEN_COMMAND supplied the credential for the preflight, the snapshot holder, pg_dump and pg_dumpall (the globals step opens a NEW connection); the private token passfile was removed with the temp dir"; else res backup_iam_token_mode_refreshes_per_connection FAIL "rc=${RC} $OUTP"; fi
rm -rf "$IAM_DIR"
expect_refused backup_iam_token_command_failure_fails_closed 'BACKUP_IAM_TOKEN_COMMAND failed' \
  env -u BACKUP_ALLOW_UNENCRYPTED -u BACKUP_PGPASSFILE BACKUP_IAM_TOKEN_COMMAND=false BACKUP_ENCRYPT_AGE_RECIPIENT="$AGE_PUB" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label iamfail
rm -f "${SECRETS}/token"

RPF="${SECRETS}/pgpass.restore"
restore_run() { # dir newdb [extra args...]  (env: RESTORE_* from caller)
  local dir="$1" nd="$2"; shift 2
  ( export BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT="${RESTORE_PORT:-$SRC_PORT}" BACKUP_PGDATABASE="${RESTORE_MAINT_DB:-postgres}" BACKUP_PGUSER="${RESTORE_USER:-drill_restore}" BACKUP_PGPASSFILE="${RESTORE_PASSFILE:-$RPF}" RESTORE_CONFIRM="${RESTORE_CONFIRM_OVERRIDE:-$nd}"
    [ -n "${RESTORE_EXPECT_SET_SHA256:-}" ] || export RESTORE_EXPECT_SET_SHA256="$(sha256sum <"$dir/SHA256SUMS" 2>/dev/null | cut -d' ' -f1)"   # the runner's out-of-band record of this very set
    "${SCRIPT_DIR}/restore.sh" --backup-dir "$dir" --new-database "$nd" "$@" ) 2>&1
}
verify_run() { # port db baseline [extra args] ; env VERIFY_*
  local port="$1" db="$2" base="$3"; shift 3
  ( export BACKUP_PGHOST=127.0.0.1 BACKUP_PGPORT="$port" BACKUP_PGDATABASE="$db" BACKUP_PGUSER=igaming_backup BACKUP_PGPASSFILE="${VERIFY_BACKUP_PASSFILE:-$BPF}"
    export VERIFY_OWNER_PGUSER=igaming VERIFY_OWNER_PGPASSFILE="${SECRETS}/pgpass"
    export VERIFY_OWNER_DATABASE_URL="postgres://igaming@127.0.0.1:${port}/${db}?sslmode=disable"      # no password in any URL: the passfile is used (L10)
    export VERIFY_RUNTIME_DATABASE_URL="postgres://igaming_runtime@127.0.0.1:${port}/${db}?sslmode=disable"
    export MIGRATE_BIN="${WORK}/migrate" RESTORECHECK_BIN="${WORK}/restorecheck"
    "${SCRIPT_DIR}/verify.sh" --baseline "$base" "$@" ) 2>&1
}

RST_DB=igaming_restored
ROUT="$(restore_run "$PLAIN_DIR" "$RST_DB")" || { echo "$ROUT" >&2; bk_die "restore failed"; }
echo "$ROUT" | grep -E '^TIMING' | tee -a "$OUT"
res logical_restore_into_new_database PASS "signature verified against the pinned key, then restored into ${RST_DB} on the same cluster as the non-superuser restore role (single transaction, no trigger/RLS bypass)"
VOUT="$(verify_run "$SRC_PORT" "$RST_DB" "${PLAIN_DIR}/baseline.txt" --retired-kid "$CANARY_KID" --retired-kid "$SEED_AP_KID")" && VRC=0 || VRC=$?
echo "$VOUT" | grep -E '^(CHK|TIMING|VERIFY)' >"${WORK}/verify-logical.txt"; grep -E '^TIMING' "${WORK}/verify-logical.txt" | tee -a "$OUT"
if [ "$VRC" -eq 0 ]; then res logical_restore_verified PASS "$(tail -1 "${WORK}/verify-logical.txt"); $(grep -c '^CHK|.*|PASS|' "${WORK}/verify-logical.txt") checks"; else res logical_restore_verified FAIL "$(grep FAIL "${WORK}/verify-logical.txt" | head -4 | cut -c1-200 | tr '\n' ' ')"; fi
# M4: the environment anchor is NOT carried by a logical restore (the source was a DEVELOPMENT database)
ANC_SRC="$(src_q 'SELECT environment FROM database_environment')"
ANC_RST="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$RST_DB" -c 'SELECT count(*) FROM database_environment')"
if [ "$ANC_SRC" = development ] && [ "$ANC_RST" = 0 ] && grep -q 'CHK|anchor_blank_after_restore|PASS' "${WORK}/verify-logical.txt" && grep -q 'CHK|startup_guard_production_process_refused_when_unrecorded|PASS' "${WORK}/verify-logical.txt"; then
  res restore_does_not_carry_environment_anchor PASS "source anchor '${ANC_SRC}', restored database_environment holds ${ANC_RST} rows; a production-process startup guard REFUSES the restored database until the owner records the environment (restorecheck: startup_guard_production_process_refused_when_unrecorded PASS)"
else res restore_does_not_carry_environment_anchor FAIL "source=${ANC_SRC} restored_rows=${ANC_RST}"; fi
# ---- owner decision 5: the restored database holds NO actor-proof secret and no proof verifies until a NEW key is provisioned ----------------------------------
# ap_probe <port> <db> <kid> <secret hex> [single|replay] : as igaming_runtime (the role the application connects as), build a PERFECT proof (right shape, actor/scope/
# tenant/operation/target/payload, fresh time window, fresh nonce) signed with <secret> under <kid> and present it to the SQL verifier actor_proof_require in a transaction that is
# never committed (no nonce row is left). Prints OK, or the SQLSTATE (AP002 = unknown/inactive key or bad MAC, AP005 = replay). `replay` presents the same proof twice.
ap_probe() {
  local out
  out="$(AP_KID="$3" AP_HEX="$4" AP_MODE="${5:-single}" psql_as igaming_runtime "$OPF" "$1" "$2" -f - 2>&1 <<'SQL' || true
\set ON_ERROR_STOP on
\set VERBOSITY sqlstate
\getenv kid AP_KID
\getenv hex AP_HEX
\getenv mode AP_MODE
SELECT gen_random_uuid() AS actor, gen_random_uuid() AS tenant, 'n' || substr(md5(random()::text || clock_timestamp()::text), 1, 24) AS nonce, extract(epoch FROM clock_timestamp())::bigint AS iat, (:'mode' = 'replay') AS rep \gset
BEGIN;
SELECT set_config('app.actor_proof', r.tok, true) FROM (SELECT q.s || '|' || encode(public.hmac(convert_to(q.s, 'UTF8'), decode(:'hex', 'hex'), 'sha256'), 'hex') AS tok
  FROM (SELECT concat_ws('|', 'v1', :'kid', :'actor', 'tenant', :'tenant', 'ledger_adjustment:initiate', 'new', repeat('0', 64), :'iat', :'iat'::bigint + 30, :'nonce') AS s) q) r;
SELECT actor_proof_require(:'actor'::uuid, 'tenant', :'tenant'::uuid, 'ledger_adjustment:initiate', 'new', repeat('0', 64));
SELECT 'FIRST_OK';
\if :rep
SAVEPOINT again;
SELECT actor_proof_require(:'actor'::uuid, 'tenant', :'tenant'::uuid, 'ledger_adjustment:initiate', 'new', repeat('0', 64));
\endif
SQL
)"
  if grep -q 'AP00[0-9]' <<<"$out"; then grep -o 'AP00[0-9]' <<<"$out" | tail -1; elif grep -q FIRST_OK <<<"$out"; then echo OK; else echo "ERR:$(head -2 <<<"$out" | tr '\n' ' ')"; fi
}
RST_AP_ROWS="$(psql_as igaming "$OPF" "$SRC_PORT" "$RST_DB" -c 'SELECT count(*) FROM actorproof_private.actor_proof_keys')"   # recreated EMPTY from actorproof-private-schema.sql
RST_LEAKS="$(pg_dump -h "$(cl_sock src)" -p "$SRC_PORT" -U postgres -Fp "$RST_DB" | leaks)"   # the WHOLE restored database as SQL text, unexcluded, as the cluster superuser
PROBE_CTRL="$(ap_probe "$SRC_PORT" "$DB" "$CANARY_KID" "$CANARY_HEX")"                      # control: on the SOURCE the very same proof is accepted (the probe is not vacuous)
PROBE_CTRL_REPLAY="$(ap_probe "$SRC_PORT" "$DB" "$CANARY_KID" "$CANARY_HEX" replay)"
PROBE_CANARY="$(ap_probe "$SRC_PORT" "$RST_DB" "$CANARY_KID" "$CANARY_HEX")"
PROBE_SEED="$(ap_probe "$SRC_PORT" "$RST_DB" "$SEED_AP_KID" "$SEED_AP_HEX")"
if [ "$RST_AP_ROWS" = 0 ] && [ "$RST_LEAKS" = 0 ] && grep -q 'recreated EMPTY from actorproof-private-schema.sql' <<<"$ROUT" && grep -q 'CHK|equal_FP|PASS' "${WORK}/verify-logical.txt" && [ "$PROBE_CTRL" = OK ] && [ "$PROBE_CTRL_REPLAY" = AP005 ] && [ "$PROBE_CANARY" = AP002 ] && [ "$PROBE_SEED" = AP002 ] \
   && grep -q 'CHK|actor_proof_keys_empty_after_logical_restore|PASS' "${WORK}/verify-logical.txt" && grep -q "CHK|actor_proof_startup_gate_refuses_${CANARY_KID}|PASS" "${WORK}/verify-logical.txt" \
   && grep -q "CHK|actor_proof_startup_gate_refuses_${SEED_AP_KID}|PASS" "${WORK}/verify-logical.txt" && grep -q 'CHK|actor_proof_verifier_fails_closed|PASS' "${WORK}/verify-logical.txt"; then
  res restored_database_has_no_actor_proof_secret_and_fails_closed PASS "restored ${RST_DB}: the schema actorproof_private was recreated from the reviewed DDL (its structure fingerprint equals the source's: equal_FP PASS) and its actor_proof_keys holds ${RST_AP_ROWS} rows and a pg_dump of the WHOLE database finds 0 occurrences of the canary or the platform key; a perfectly-formed proof signed with the source's canary key and with the source's platform key is refused with AP002 (control on the source: accepted, and its replay refused with AP005); verify.sh/restorecheck: empty table, the platform's production startup gate refuses both source kids, a well-formed proof is refused with AP002"
else res restored_database_has_no_actor_proof_secret_and_fails_closed FAIL "rows=${RST_AP_ROWS} leaks=${RST_LEAKS} control=${PROBE_CTRL}/${PROBE_CTRL_REPLAY} canary=${PROBE_CANARY} seed=${PROBE_SEED} $(grep -h 'actor_proof' "${WORK}/verify-logical.txt" | cut -c1-160 | tr '\n' ' ')"; fi
# the restore's own output (the "NEXT" text) names the re-key step, and the restore output holds no secret
if grep -q 'rekey-actor-proof.sh' <<<"$ROUT" && [ "$(printf '%s\n' "$ROUT" | leaks)" = 0 ]; then res restore_output_names_the_rekey_step_and_leaks_nothing PASS "restore.sh's closing instructions name deploy/backup/rekey-actor-proof.sh and the AP002 / refusal-to-start consequence; its output holds no secret form"; else res restore_output_names_the_rekey_step_and_leaks_nothing FAIL "$(printf '%s' "$ROUT" | tail -3 | tr '\n' ' ')"; fi
# a FOREIGN set (not made by backup.sh: a superuser dump of the source, which carries the whole schema actorproof_private WITH its rows) is restored without any of
# it, with warnings; the private schema is recreated EMPTY from the reviewed DDL (restore skips every TOC entry of that schema, even from a validly signed set)
cp -a "$PLAIN_DIR" "${BKUP}/legacy"
pg_dump -h "$(cl_sock src)" -p "$SRC_PORT" -U postgres -Fc -f "${BKUP}/legacy/db.dump" "$DB"       # NO exclusion, as the superuser
rehash_manifest "${BKUP}/legacy"; resign "${BKUP}/legacy"                                          # a validly re-sealed (signed) set
LEGACY_TOC_DATA="$(pg_restore --list "${BKUP}/legacy/db.dump" | grep -c ' TABLE DATA actorproof_private actor_proof_keys ' || true)"
LOUT_LG="$(restore_run "${BKUP}/legacy" legacy_db)" && LGRC=0 || LGRC=$?
LG_ROWS="$(psql_as igaming "$OPF" "$SRC_PORT" legacy_db -c 'SELECT count(*) FROM actorproof_private.actor_proof_keys' 2>/dev/null || echo ERR)"
LG_LEAKS="$(pg_dump -h "$(cl_sock src)" -p "$SRC_PORT" -U postgres -Fp legacy_db 2>/dev/null | leaks)"
if [ "$LEGACY_TOC_DATA" = 1 ] && [ "$LGRC" -eq 0 ] && [ "$LG_ROWS" = 0 ] && [ "$LG_LEAKS" = 0 ] && grep -q 'legacy set' <<<"$LOUT_LG" && grep -q 'NOT made by backup.sh' <<<"$LOUT_LG"; then
  res legacy_set_with_actor_proof_rows_is_restored_without_them PASS "control: a validly signed set whose archive DOES carry the schema actorproof_private with its rows (a superuser dump) restores with both warnings and none of it: the recreated actorproof_private.actor_proof_keys holds ${LG_ROWS} rows and a pg_dump of the restored database finds ${LG_LEAKS} secret occurrences"
else res legacy_set_with_actor_proof_rows_is_restored_without_them FAIL "toc_data=${LEGACY_TOC_DATA} rc=${LGRC} rows=${LG_ROWS} leaks=${LG_LEAKS} $(printf '%s' "$LOUT_LG" | tail -2 | tr '\n' ' ')"; fi
psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c 'DROP DATABASE IF EXISTS legacy_db WITH (FORCE)' >/dev/null 2>&1 || true
rm -rf "${BKUP}/legacy"
# ---- F1 (migration 0166): a TRUE legacy database / set (migration < 0166: the key table, rows included, in schema public) ---------------------
# A throwaway database is migrated to 0165 only, given the backup role's blanket grant (as init-backup-role.sql gives it) and two planted keys. Then:
#   (1) migration 0166 up moves the rows into actorproof_private and strips the backup role's grant; down restores the 0165 catalog and the rows byte for
#       byte (incl. the backup role's table-level SELECT, reconstructed from its SELECT on actor_proof_nonces); up again equals the first up;
#   (2) its superuser dump at 0165 (what an old backup tool produced from an old database) is a validly re-sealed legacy set: restore.sh skips the rows,
#       keeps the LEGACY layout (an EMPTY public.actor_proof_keys, no private schema) and `migrate up` then moves the empty table.
LEG_DB=legacy_src
mig_dir() { # <max version> -> a private copy of the migrations up to that version
  local d="${WORK}/migrations-$1" f n
  mkdir -p "$d"
  for f in "${REPO_ROOT}"/migrations/*.sql; do n="$(basename "$f")"; if [ "$((10#${n:0:4}))" -le "$1" ]; then cp "$f" "$d/"; fi; done
  printf '%s' "$d"
}
OWNER_PW="$(pw_of "${SECRETS}/pgpass" "$SRC_PORT" igaming)"
leg_url() { printf 'postgres://igaming:%s@127.0.0.1:%s/%s?sslmode=disable' "$OWNER_PW" "$SRC_PORT" "$1"; }
# catalog fingerprint of everything 0166 touches, by name (as the throwaway cluster's superuser)
leg_snap() {
  cl_super src "$SRC_PORT" -d "$1" -c "SELECT md5(string_agg(x, E'\n' ORDER BY x)) FROM (
      SELECT 'ns:' || nspname || ':' || COALESCE(nspacl::text, '') AS x FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
      UNION ALL SELECT 'rel:' || n.nspname || '.' || c.relname || ':' || c.relkind::text || ':' || COALESCE(c.relacl::text, '') || ':' || c.relrowsecurity || ':' || c.relforcerowsecurity
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relname LIKE 'actor_proof%'
      UNION ALL SELECT 'col:' || c.relname || '.' || a.attname || ':' || a.attnum || ':' || format_type(a.atttypid, a.atttypmod) || ':' || a.attnotnull || ':' || COALESCE(a.attacl::text, '')
        FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid WHERE c.relname = 'actor_proof_keys' AND a.attnum > 0 AND NOT a.attisdropped
      UNION ALL SELECT 'fn:' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || '):' || md5(p.prosrc) || ':' || COALESCE(p.proacl::text, '') || ':'
                       || COALESCE(array_to_string(p.proconfig, ','), '') || ':' || p.prosecdef
        FROM pg_proc p WHERE p.proname LIKE 'actor_proof%'
      UNION ALL SELECT 'con:' || c.relname || ':' || co.conname || ':' || pg_get_constraintdef(co.oid) FROM pg_constraint co JOIN pg_class c ON c.oid = co.conrelid WHERE c.relname = 'actor_proof_keys') q"
}
leg_keys() { # row count and md5 over every key row (kid, status, times, md5(secret)), wherever the table is
  local tbl
  tbl="$(cl_super src "$SRC_PORT" -d "$1" -c "SELECT COALESCE(to_regclass('actorproof_private.actor_proof_keys')::text, to_regclass('public.actor_proof_keys')::text)")"
  cl_super src "$SRC_PORT" -d "$1" -c "SELECT count(*) || ':' || COALESCE(md5(string_agg(kid || '/' || status || '/' || created_at || '/' || COALESCE(retired_at::text, '') || '/' || md5(secret), ',' ORDER BY kid)), '') FROM ${tbl}"
}
leg_bk() { # does igaming_backup hold any privilege on the key table / its schema (by OID)?
  cl_super src "$SRC_PORT" -d "$1" -c "SELECT (has_table_privilege('igaming_backup', c.oid, 'SELECT') OR (n.nspname <> 'public' AND has_schema_privilege('igaming_backup', n.oid, 'USAGE')))::int
                                          FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relname = 'actor_proof_keys' AND c.relkind = 'r'"
}
cl_super src "$SRC_PORT" -d postgres -c "CREATE DATABASE ${LEG_DB} OWNER igaming TEMPLATE template0" >/dev/null
M165="$(mig_dir 165)"; M166="$(mig_dir 166)"
DATABASE_URL="$(leg_url "$LEG_DB")" "${WORK}/migrate" -dir "$M165" up >"${WORK}/leg-migrate.log" 2>&1 || { tail -5 "${WORK}/leg-migrate.log" >&2; bk_die "legacy database: migrate to 0165 failed"; }
IGPW="$CANARY" CANARY_KID_V="$CANARY_KID" cl_super src "$SRC_PORT" -d "$LEG_DB" <<'SQL' >/dev/null
\getenv cn IGPW
\getenv ck CANARY_KID_V
GRANT SELECT ON ALL TABLES IN SCHEMA public TO igaming_backup;
SET ROLE igaming;
INSERT INTO public.actor_proof_keys (kid, secret) VALUES (:'ck', convert_to(:'cn', 'UTF8'));
INSERT INTO public.actor_proof_keys (kid, secret, status, retired_at) VALUES ('ap-legacy-retired', decode(repeat('5a', 40), 'hex'), 'retired', now());
SQL
L_SNAP0="$(leg_snap "$LEG_DB")"; L_KEYS0="$(leg_keys "$LEG_DB")"; L_BK0="$(leg_bk "$LEG_DB")"
DATABASE_URL="$(leg_url "$LEG_DB")" "${WORK}/migrate" -dir "$M166" up >>"${WORK}/leg-migrate.log" 2>&1 && L_UP=0 || L_UP=$?
L_SNAP1="$(leg_snap "$LEG_DB")"; L_KEYS1="$(leg_keys "$LEG_DB")"; L_BK1="$(leg_bk "$LEG_DB")"
L_PUB1="$(cl_super src "$SRC_PORT" -d "$LEG_DB" -c "SELECT (to_regclass('public.actor_proof_keys') IS NULL AND to_regclass('actorproof_private.actor_proof_keys') IS NOT NULL)::int")"
L_PROBE="$(ap_probe "$SRC_PORT" "$LEG_DB" "$CANARY_KID" "$CANARY_HEX")"                  # the moved key still verifies (as igaming_runtime)
DATABASE_URL="$(leg_url "$LEG_DB")" "${WORK}/migrate" -dir "$M166" -steps 1 down >>"${WORK}/leg-migrate.log" 2>&1 && L_DOWN=0 || L_DOWN=$?
L_SNAP2="$(leg_snap "$LEG_DB")"; L_KEYS2="$(leg_keys "$LEG_DB")"; L_BK2="$(leg_bk "$LEG_DB")"
DATABASE_URL="$(leg_url "$LEG_DB")" "${WORK}/migrate" -dir "$M166" up >>"${WORK}/leg-migrate.log" 2>&1 && L_UP2=0 || L_UP2=$?
L_SNAP3="$(leg_snap "$LEG_DB")"; L_KEYS3="$(leg_keys "$LEG_DB")"
if [ "$L_UP" = 0 ] && [ "$L_DOWN" = 0 ] && [ "$L_UP2" = 0 ] && [ "$L_BK0" = 1 ] && [ "$L_BK1" = 0 ] && [ "$L_BK2" = 1 ] && [ "$L_PUB1" = 1 ] && [ "$L_PROBE" = OK ] \
   && [ "${L_KEYS0%%:*}" = 2 ] && [ "$L_KEYS1" = "$L_KEYS0" ] && [ "$L_KEYS2" = "$L_KEYS0" ] && [ "$L_KEYS3" = "$L_KEYS0" ] \
   && [ "$L_SNAP2" = "$L_SNAP0" ] && [ "$L_SNAP3" = "$L_SNAP1" ] && [ "$L_SNAP1" != "$L_SNAP0" ]; then
  res migration_0166_up_down_up_on_a_backup_granted_database PASS "0165 database with the backup role's blanket SELECT and 2 key rows: up moved the rows intact into actorproof_private (row digest unchanged) and removed every backup-role privilege on the key; the moved canary key still verifies a proof as igaming_runtime; down restored the 0165 catalog byte for byte (incl. igaming_backup's SELECT) with the rows intact; up again equals the first up"
else res migration_0166_up_down_up_on_a_backup_granted_database FAIL "up=${L_UP} down=${L_DOWN} up2=${L_UP2} bk=${L_BK0}/${L_BK1}/${L_BK2} moved=${L_PUB1} probe=${L_PROBE} keys=${L_KEYS0}|${L_KEYS1}|${L_KEYS2}|${L_KEYS3} snap0=${L_SNAP0} snap1=${L_SNAP1} snap2=${L_SNAP2} snap3=${L_SNAP3} $(tail -3 "${WORK}/leg-migrate.log" | tr '\n' ' ')"; fi
# (2) back to 0165 for the legacy set, then dump it the way an old tool did (no exclusion) and re-seal it as a valid signed set
DATABASE_URL="$(leg_url "$LEG_DB")" "${WORK}/migrate" -dir "$M166" -steps 1 down >>"${WORK}/leg-migrate.log" 2>&1 || bk_die "legacy database: migrate down to 0165 failed"
cp -a "$PLAIN_DIR" "${BKUP}/legacy165"
pg_dump -h "$(cl_sock src)" -p "$SRC_PORT" -U postgres -Fc -f "${BKUP}/legacy165/db.dump" "$LEG_DB"
rehash_manifest "${BKUP}/legacy165"; resign "${BKUP}/legacy165"
L_TOC_DATA="$(pg_restore --list "${BKUP}/legacy165/db.dump" | grep -c ' TABLE DATA public actor_proof_keys ' || true)"
L_ROUT="$(restore_run "${BKUP}/legacy165" legacy165_db)" && L_RRC=0 || L_RRC=$?
L_RROWS="$(psql_as igaming "$OPF" "$SRC_PORT" legacy165_db -c 'SELECT count(*) FROM public.actor_proof_keys' 2>/dev/null || echo ERR)"
L_RSCHEMA="$(psql_as igaming "$OPF" "$SRC_PORT" legacy165_db -c "SELECT (to_regnamespace('actorproof_private') IS NOT NULL)::int" 2>/dev/null || echo ERR)"
L_RLEAKS="$(pg_dump -h "$(cl_sock src)" -p "$SRC_PORT" -U postgres -Fp legacy165_db 2>/dev/null | leaks)"
DATABASE_URL="$(leg_url legacy165_db)" "${WORK}/migrate" -dir "${REPO_ROOT}/migrations" up >>"${WORK}/leg-migrate.log" 2>&1 && L_RUP=0 || L_RUP=$?
L_RMOVED="$(psql_as igaming "$OPF" "$SRC_PORT" legacy165_db -c "SELECT (to_regclass('public.actor_proof_keys') IS NULL)::int || ':' || (SELECT count(*) FROM actorproof_private.actor_proof_keys)" 2>/dev/null || echo ERR)"
if [ "$L_TOC_DATA" = 1 ] && [ "$L_RRC" = 0 ] && grep -q 'legacy set' <<<"$L_ROUT" && grep -q 'LEGACY layout' <<<"$L_ROUT" && [ "$L_RROWS" = 0 ] && [ "$L_RSCHEMA" = 0 ] \
   && [ "$L_RLEAKS" = 0 ] && [ "$L_RUP" = 0 ] && [ "$L_RMOVED" = "1:0" ] && [ "$(printf '%s\n' "$L_ROUT" | leaks)" = 0 ]; then
  res true_legacy_set_is_restored_without_rows_and_migrates PASS "a validly signed set dumped from a 0165 database (TABLE DATA public actor_proof_keys present) restores with the 'legacy set' warning in the LEGACY layout: public.actor_proof_keys EMPTY, no private schema, 0 secret occurrences in a superuser dump of the result; migrate up then moved the EMPTY table into actorproof_private"
else res true_legacy_set_is_restored_without_rows_and_migrates FAIL "toc_data=${L_TOC_DATA} rc=${L_RRC} rows=${L_RROWS} schema=${L_RSCHEMA} leaks=${L_RLEAKS} up=${L_RUP} moved=${L_RMOVED} $(printf '%s' "$L_ROUT" | tail -2 | tr '\n' ' ')"; fi
psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c 'DROP DATABASE IF EXISTS legacy165_db WITH (FORCE)' >/dev/null 2>&1 || true
cl_super src "$SRC_PORT" -d postgres -c "DROP DATABASE IF EXISTS legacy_src WITH (FORCE)" >/dev/null 2>&1 || true
rm -rf "${BKUP}/legacy165" "$M165" "$M166"

# the restore must state the source environment it believes it restores
OUTP="$(RESTORE_EXPECT_SOURCE_ENVIRONMENT=production restore_run "$PLAIN_DIR" m4_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'RESTORE_EXPECT_SOURCE_ENVIRONMENT must equal' && [ "$(psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c "SELECT count(*) FROM pg_database WHERE datname = 'm4_db'")" = 0 ]; then res restore_refuses_wrong_expected_source_environment PASS "restoring a 'development' backup while asserting 'production' is refused; no database created"; else res restore_refuses_wrong_expected_source_environment FAIL "rc=${RC} $OUTP"; fi
# encrypted artifact: same restore (decrypt with the ephemeral identity), compare counts only
RESTORE_AGE_IDENTITY_FILE="${SECRETS}/age.key" restore_run "$ENC_DIR" igaming_restored_enc >"${WORK}/enc-restore.txt" && ENCRC=0 || ENCRC=$?
if [ "$ENCRC" -eq 0 ]; then
  ENC_COUNT="$(psql_as igaming_backup "$BPF" "$SRC_PORT" igaming_restored_enc -c 'SELECT count(*) FROM ledger_entries')"
  [ "$ENC_COUNT" = "$ENTRIES" ] && res encrypted_backup_restores PASS "age-encrypted backup decrypted and restored: ledger_entries=${ENC_COUNT}" || res encrypted_backup_restores FAIL "count ${ENC_COUNT} != ${ENTRIES}"
else res encrypted_backup_restores FAIL "$(tail -3 "${WORK}/enc-restore.txt" | tr '\n' ' ')"; fi
psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c 'DROP DATABASE IF EXISTS igaming_restored_enc WITH (FORCE)' >/dev/null 2>&1 || true
if [ -z "$(find "$BACKUP_TMPDIR" -mindepth 1 2>/dev/null)" ]; then res restore_leaves_no_decrypted_material PASS "after an encrypted restore the controlled temp root is empty (decrypted dump/globals/baseline removed)"; else res restore_leaves_no_decrypted_material FAIL "$(ls -A "$BACKUP_TMPDIR" | tr '\n' ' ')"; fi

# ================================================================= phase 5a: negative control + failure handling (backup/restore side)
section "5 negative control and failure handling"
# negative control: damage the restored copy (own throwaway superuser, guard bypass is DRILL ONLY) -> verify must FAIL
cl_super src "$SRC_PORT" -d "$RST_DB" -c "SET session_replication_role = replica; UPDATE ledger_entries SET amount = amount + 7 WHERE id = (SELECT id FROM ledger_entries ORDER BY id LIMIT 1)" >/dev/null
if NOUT="$(verify_run "$SRC_PORT" "$RST_DB" "${PLAIN_DIR}/baseline.txt" 2>&1)"; then res negative_control_verify_detects_tampered_ledger FAIL "verify PASSED on a damaged restore (vacuous verifier)"; else
  NDET="$(printf '%s\n' "$NOUT" | grep -E '^CHK\|(equal_TABLE|invariants_all_zero)\|FAIL' | cut -d'|' -f2 | tr '\n' ' ')"
  res negative_control_verify_detects_tampered_ledger PASS "verify FAILED as required on a copy with one ledger amount +7; detected by: ${NDET}"; fi
cl_super src "$SRC_PORT" -d "$RST_DB" -c "SET session_replication_role = replica; UPDATE ledger_entries SET amount = amount - 7 WHERE id = (SELECT id FROM ledger_entries ORDER BY id LIMIT 1)" >/dev/null
VOUT2="$(verify_run "$SRC_PORT" "$RST_DB" "${PLAIN_DIR}/baseline.txt" 2>&1)" && res negative_control_reverted_verify_passes_again PASS "after undoing the damage verify passes again (the failure was the damage, nothing else)" || res negative_control_reverted_verify_passes_again FAIL "$(printf '%s\n' "$VOUT2" | grep FAIL | head -3 | tr '\n' ' ')"

# ---- owner decision 5: RE-KEY the restored database through the scripted procedure (deploy/backup/rekey-actor-proof.sh = operational-runbooks.md section 15 step 4) --------
# (runs on ${RST_DB} AFTER the negative-control pair above, which needs the unprovisioned state)
REKEY_KID=ap-rekey-1
NEWHEX_FILE="${SECRETS}/ap-rekey.hex"
(umask 077; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$NEWHEX_FILE"); chmod 600 "$NEWHEX_FILE"
NEWHEX="$(cat "$NEWHEX_FILE")"
rekey_run() { env REKEY_PGHOST=127.0.0.1 REKEY_PGPORT="$SRC_PORT" REKEY_PGDATABASE="${REKEY_DB:-$RST_DB}" REKEY_PGUSER="${REKEY_USER:-igaming}" REKEY_PGPASSFILE="$OPF" "${SCRIPT_DIR}/rekey-actor-proof.sh" "$@" 2>&1; }
ap_rows() { psql_as igaming "$OPF" "$SRC_PORT" "$RST_DB" -c 'SELECT count(*) FROM actorproof_private.actor_proof_keys'; }
REKEY_NEG=""
# (1) the source's key id cannot be reused
OUTP="$(rekey_run --kid "$SEED_AP_KID" --secret-hex-file "$NEWHEX_FILE" --retired-kid "$SEED_AP_KID")" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q 'equals a retired kid' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} reuse_of_retired_kid($RC)"
# (2) a world-readable secret file
cp "$NEWHEX_FILE" "${SECRETS}/ap-open.hex"; chmod 644 "${SECRETS}/ap-open.hex"
OUTP="$(rekey_run --kid "$REKEY_KID" --retired-kid "$SEED_AP_KID" --secret-hex-file "${SECRETS}/ap-open.hex")" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q 'must be mode 0600' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} open_file($RC)"
rm -f "${SECRETS}/ap-open.hex"
# (3) a too-short secret
printf 'abcd' >"${SECRETS}/ap-short.hex"; chmod 600 "${SECRETS}/ap-short.hex"
OUTP="$(rekey_run --kid "$REKEY_KID" --retired-kid "$SEED_AP_KID" --secret-hex-file "${SECRETS}/ap-short.hex")" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q 'even-length hex' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} short_secret($RC)"
rm -f "${SECRETS}/ap-short.hex"
# (4) not the table owner: the runtime role
OUTP="$(REKEY_USER=igaming_runtime rekey_run --kid "$REKEY_KID" --retired-kid "$SEED_AP_KID" --secret-hex-file "$NEWHEX_FILE")" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q 'cannot read actorproof_private.actor_proof_keys' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} runtime_role($RC)"
# (5) statement logging that could capture the bound secret
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming SET log_statement = 'all'" >/dev/null
OUTP="$(rekey_run --kid "$REKEY_KID" --retired-kid "$SEED_AP_KID" --secret-hex-file "$NEWHEX_FILE")" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q 'log_statement=all' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} statement_logging($RC)"
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming RESET log_statement" >/dev/null
# (6) sampled-transaction logging writes every statement of a sampled transaction WITH its parameters
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming SET log_transaction_sample_rate = 1" >/dev/null
OUTP="$(rekey_run --kid "$REKEY_KID" --retired-kid "$SEED_AP_KID" --secret-hex-file "$NEWHEX_FILE")" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q 'log_transaction_sample_rate' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} txn_sampling($RC)"
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming RESET log_transaction_sample_rate" >/dev/null
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming SET log_statement_sample_rate = 1" -c "ALTER ROLE igaming SET log_min_duration_sample = 0" >/dev/null
OUTP="$(rekey_run --kid "$REKEY_KID" --retired-kid "$SEED_AP_KID" --secret-hex-file "$NEWHEX_FILE")" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q 'log_statement_sample_rate' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} stmt_sampling($RC)"
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming RESET log_statement_sample_rate" -c "ALTER ROLE igaming RESET log_min_duration_sample" >/dev/null
# (7) no source kid named, and no --first-key
OUTP="$(rekey_run --kid "$REKEY_KID" --secret-hex-file "$NEWHEX_FILE")" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q -e '--retired-kid' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} no_retired_kid($RC)"
# (8) verify.sh refuses to skip the actor-proof assertion after a LOGICAL restore
OUTP="$(verify_run "$SRC_PORT" "$RST_DB" "${PLAIN_DIR}/baseline.txt" --expect-actor-proof skip 2>&1)" && RC=0 || RC=$?; { [ "$RC" -ne 0 ] && grep -q 'allowed only with --anchor-mode preserved' <<<"$OUTP"; } || REKEY_NEG="${REKEY_NEG} verify_skip($RC)"
if [ -z "$REKEY_NEG" ] && [ "$(ap_rows)" = 0 ]; then res rekey_script_refuses_unsafe_use PASS "refused, table still empty: reuse of the source kid, a world-readable secret file, a too-short secret, a non-owner role (igaming_runtime), log_statement=all, log_transaction_sample_rate, sampled statement logging, no --retired-kid; and verify.sh refuses --expect-actor-proof skip after a logical restore"; else res rekey_script_refuses_unsafe_use FAIL "not refused:${REKEY_NEG} rows=$(ap_rows)"; fi
# the real re-key
LOGBYTES_RK="$(stat -c %s "${DRILL_ROOT}/src/server.log")"
ROUT_RK="$(rekey_run --kid "$REKEY_KID" --retired-kid "$SEED_AP_KID" --secret-hex-file "$NEWHEX_FILE" --retired-kid "$CANARY_KID" --retired-kid "$SEED_AP_KID")" && RKRC=0 || RKRC=$?
ROUT_RK2="$(rekey_run --kid ap-rekey-2 --retired-kid "$SEED_AP_KID" --secret-hex-file "$NEWHEX_FILE")" && RKRC2=0 || RKRC2=$?
RK_LOG_LEAK="$( { tail -c +"$((LOGBYTES_RK + 1))" "${DRILL_ROOT}/src/server.log"; printf '%s\n%s\n' "$ROUT_RK" "$ROUT_RK2"; } | grep -a -c -F "$NEWHEX" || true)"
if [ "$RKRC" -eq 0 ] && grep -q "probe_active=true" <<<"$ROUT_RK" && [ "$RKRC2" -ne 0 ] && grep -q 'already holds 1 key' <<<"$ROUT_RK2" && [ "$(ap_rows)" = 1 ] && [ "$RK_LOG_LEAK" = 0 ]; then
  res rekey_script_provisions_new_key PASS "kid ${REKEY_KID} inserted as the owner with the secret as a bound parameter (psql read it from a 0600 file; not in argv, statement text or the server log: ${RK_LOG_LEAK} occurrences in the log and script output); the SECURITY DEFINER probe reports it active; a second run is refused ('already holds 1 key': first-key provisioning only)"
else res rekey_script_provisions_new_key FAIL "rc=${RKRC} rc2=${RKRC2} rows=$(ap_rows) logleak=${RK_LOG_LEAK} $(printf '%s' "$ROUT_RK" | tail -2 | tr '\n' ' ')"; fi
# proof-bound operations after the re-key (SQL level: the application's verifier path as igaming_runtime; see the record for why not the full HTTP flow)
Q_NEW="$(ap_probe "$SRC_PORT" "$RST_DB" "$REKEY_KID" "$NEWHEX")"
Q_NEW_REPLAY="$(ap_probe "$SRC_PORT" "$RST_DB" "$REKEY_KID" "$NEWHEX" replay)"
Q_OLD_CANARY="$(ap_probe "$SRC_PORT" "$RST_DB" "$CANARY_KID" "$CANARY_HEX")"
Q_OLD_SEED="$(ap_probe "$SRC_PORT" "$RST_DB" "$SEED_AP_KID" "$SEED_AP_HEX")"
Q_OLD_UNDER_NEW_KID="$(ap_probe "$SRC_PORT" "$RST_DB" "$REKEY_KID" "$CANARY_HEX")"
Q_NEW_UNDER_OLD_KID="$(ap_probe "$SRC_PORT" "$RST_DB" "$CANARY_KID" "$NEWHEX")"
if [ "$Q_NEW" = OK ] && [ "$Q_NEW_REPLAY" = AP005 ] && [ "$Q_OLD_CANARY" = AP002 ] && [ "$Q_OLD_SEED" = AP002 ] && [ "$Q_OLD_UNDER_NEW_KID" = AP002 ] && [ "$Q_NEW_UNDER_OLD_KID" = AP002 ]; then
  res proof_verification_works_only_with_the_new_key_after_rekey PASS "after the re-key, as igaming_runtime: a proof under ${REKEY_KID} signed with the new secret is accepted (replay refused AP005); the source's canary and platform keys are still refused (AP002), as is the old secret relabelled with the new kid and the new secret relabelled with an old kid (AP002). SQL-level (actor_proof_require with a throwaway key), not the full HTTP K2/K3 flow"
else res proof_verification_works_only_with_the_new_key_after_rekey FAIL "new=${Q_NEW} replay=${Q_NEW_REPLAY} old_canary=${Q_OLD_CANARY} old_seed=${Q_OLD_SEED} old_under_new_kid=${Q_OLD_UNDER_NEW_KID} new_under_old_kid=${Q_NEW_UNDER_OLD_KID}"; fi
VOUT_RK="$(verify_run "$SRC_PORT" "$RST_DB" "${PLAIN_DIR}/baseline.txt" --expect-actor-proof provisioned --actor-proof-kid "$REKEY_KID" --retired-kid "$CANARY_KID" --retired-kid "$SEED_AP_KID" 2>&1)" && VRC_RK=0 || VRC_RK=$?
echo "$VOUT_RK" | grep -E '^(CHK|VERIFY)' >"${WORK}/verify-rekeyed.txt"
if [ "$VRC_RK" -eq 0 ] && grep -q 'CHK|actor_proof_key_is_the_new_one|PASS' "${WORK}/verify-rekeyed.txt" && grep -q 'CHK|actor_proof_startup_gate_admits_new_kid|PASS' "${WORK}/verify-rekeyed.txt" \
   && grep -q "CHK|actor_proof_startup_gate_refuses_retired_${CANARY_KID}|PASS" "${WORK}/verify-rekeyed.txt"; then
  res rekeyed_restore_verified PASS "$(tail -1 "${WORK}/verify-rekeyed.txt"); incl. exactly one ACTIVE key (the new kid), the source's kids absent, the platform's production startup gate admits the new kid and refuses both retired ones"
else res rekeyed_restore_verified FAIL "$(grep FAIL "${WORK}/verify-rekeyed.txt" | head -4 | cut -c1-200 | tr '\n' ' ')"; fi
# a backup of the RE-KEYED database must not carry the new secret either (the exclusion is unconditional)
RKB="$(BACKUP_PGDATABASE="$RST_DB" "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label rekeyed 2>&1)" && RKBRC=0 || RKBRC=$?
RKB_DIR="$(printf '%s\n' "$RKB" | sed -n 's/^RUN_DIR=//p')"
if [ "$RKBRC" -eq 0 ] && [ -d "$RKB_DIR" ]; then
  RKB_LEAK=$(( $(pg_restore -f - "${RKB_DIR}/db.dump" | grep -a -c -F -e "$NEWHEX" -e "$CANARY" || true) + $(grep -a -c -F -e "$NEWHEX" "${RKB_DIR}/db.dump" "${RKB_DIR}/globals.sql" "${RKB_DIR}/baseline.txt" "${RKB_DIR}/manifest.json" | awk -F: '{n += $NF} END {print n+0}') + $(printf '%s\n' "$RKB" | grep -a -c -F "$NEWHEX" || true) ))
  if [ "$RKB_LEAK" = 0 ] && ! pg_restore --list "${RKB_DIR}/db.dump" | grep -q -E ' TABLE DATA [^ ]+ actor_proof_key[^ ]* |(^| )actorproof_private( |$)'; then res backup_of_rekeyed_database_excludes_the_new_secret PASS "a backup taken after the re-key has no actor_proof_keys data entry, no entry of the schema actorproof_private and 0 occurrences of the new secret in the dump (text and raw), globals, baseline, manifest or the run output"; else res backup_of_rekeyed_database_excludes_the_new_secret FAIL "leaks=${RKB_LEAK}"; fi
else res backup_of_rekeyed_database_excludes_the_new_secret FAIL "rc=${RKBRC} $(printf '%s' "$RKB" | tail -2 | tr '\n' ' ')"; fi
[ -z "$RKB_DIR" ] || rm -rf "$RKB_DIR"
rm -f "$NEWHEX_FILE"

db_exists() { psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c "SELECT count(*) FROM pg_database WHERE datname = '$1'"; }
# F1 corrupted dump (bit flip in the middle)
cp -a "$PLAIN_DIR" "${BKUP}/f1"; SZ=$(stat -c %s "${BKUP}/f1/db.dump"); printf '\xff\xfe\xfd\xfc' | dd of="${BKUP}/f1/db.dump" bs=1 seek=$((SZ / 2)) conv=notrunc status=none
OUTP="$(restore_run "${BKUP}/f1" f1_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(printf '%s' "$OUTP" | grep -c 'CHECKSUM MISMATCH')" -ge 1 ] && [ "$(db_exists f1_db)" = 0 ]; then res failure_corrupted_dump_detected_before_restore PASS "checksum mismatch reported, exit ${RC}, no database created"; else res failure_corrupted_dump_detected_before_restore FAIL "rc=${RC} $OUTP"; fi
# F2 truncated dump (checksum not updated)
cp -a "$PLAIN_DIR" "${BKUP}/f2"; truncate -s $((SZ / 2)) "${BKUP}/f2/db.dump"
OUTP="$(restore_run "${BKUP}/f2" f2_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(printf '%s' "$OUTP" | grep -c 'CHECKSUM MISMATCH')" -ge 1 ] && [ "$(db_exists f2_db)" = 0 ]; then res failure_truncated_dump_detected_by_checksum PASS "checksum mismatch, exit ${RC}, no database created"; else res failure_truncated_dump_detected_by_checksum FAIL "rc=${RC} $OUTP"; fi
# F2b truncated dump re-sealed with a VALID signature (damage that happened before sealing): pg_restore refuses it up front
cp -a "$PLAIN_DIR" "${BKUP}/f2b"; truncate -s $((SZ * 6 / 10)) "${BKUP}/f2b/db.dump"; rehash_manifest "${BKUP}/f2b"; resign "${BKUP}/f2b"
OUTP="$(restore_run "${BKUP}/f2b" f2b_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(db_exists f2b_db)" = 0 ]; then res failure_truncated_dump_with_valid_checksum_refused PASS "pg_restore refused the unreadable archive (${OUTP##*backup: }); exit ${RC}; no database created"; else res failure_truncated_dump_with_valid_checksum_refused FAIL "rc=${RC} exists=$(db_exists f2b_db) $OUTP"; fi
# F2c bit-flipped dump re-sealed with a valid signature: the checksum cannot help. Where the damage is decides WHICH layer stops it: the archive's block structure
# (pg_restore --list refuses up front) or its zlib/COPY decoding MID-restore (the single transaction rolls back and the half-created database is dropped). A flip can
# also land in bytes pg_restore never reads (observed: restored data identical), so candidate positions near the end of the data area are tried until one fails MID-restore.
F2C_STAGE="none"; F2C_PCT=""
for pct in 95 92 90 97 88 85; do
  rm -rf "${BKUP}/f2c"; cp -a "$PLAIN_DIR" "${BKUP}/f2c"
  printf '\xde\xad\xbe\xef\x00\x11\x22\x33' | dd of="${BKUP}/f2c/db.dump" bs=1 seek=$((SZ * pct / 100)) conv=notrunc status=none
  rehash_manifest "${BKUP}/f2c"; resign "${BKUP}/f2c"
  OUTP="$(restore_run "${BKUP}/f2c" f2c_db)" && RC=0 || RC=$?
  F2C_PCT="$pct"
  if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'pg_restore failed'; then F2C_STAGE="mid-restore"; break; fi
  if [ "$RC" -ne 0 ]; then F2C_STAGE="refused-up-front"; else F2C_STAGE="NOT-DETECTED"; break; fi
done
if [ "$F2C_STAGE" != "NOT-DETECTED" ] && [ "$RC" -ne 0 ] && [ "$(db_exists f2c_db)" = 0 ]; then
  res failure_corrupt_dump_with_valid_checksum_rolled_back PASS "8 bytes flipped at ${F2C_PCT}% of a validly-signed archive: ${F2C_STAGE} ($(printf '%s' "$OUTP" | grep -m1 -E 'pg_restore failed|cannot read the dump' | cut -c1-170)); exit ${RC}; no database left (single transaction rolled back / half-created database dropped)"
else res failure_corrupt_dump_with_valid_checksum_rolled_back FAIL "stage=${F2C_STAGE} pct=${F2C_PCT} rc=${RC} exists=$(db_exists f2c_db) $OUTP"; fi
rm -rf "${BKUP}/f1" "${BKUP}/f2" "${BKUP}/f2b" "${BKUP}/f2c"
# F3 wrong role (the maintenance database is the platform database: CONNECT on postgres is revoked from PUBLIC by the hardening)
for wr in igaming_runtime igaming_backup; do
  if [ "$wr" = igaming_runtime ]; then PF="${SECRETS}/pgpass"; else PF="$BPF"; fi
  OUTP="$(RESTORE_MAINT_DB=$DB RESTORE_USER=$wr RESTORE_PASSFILE=$PF restore_run "$PLAIN_DIR" f3_db)" && RC=0 || RC=$?
  if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q "restoring as ${wr} is refused" && [ "$(db_exists f3_db)" = 0 ]; then res "failure_restore_as_${wr}_refused" PASS "clear refusal, exit ${RC}, no database created"; else res "failure_restore_as_${wr}_refused" FAIL "rc=${RC} $OUTP"; fi
done
# F4 existing database (non-empty) refused and untouched
BEFORE="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$RST_DB" -c 'SELECT count(*) FROM ledger_entries')"
OUTP="$(restore_run "$PLAIN_DIR" "$RST_DB")" && RC=0 || RC=$?
AFTER="$(psql_as igaming_backup "$BPF" "$SRC_PORT" "$RST_DB" -c 'SELECT count(*) FROM ledger_entries')"
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'already exists' && [ "$BEFORE" = "$AFTER" ]; then res failure_restore_into_existing_database_refused PASS "refused (database exists), existing ledger_entries unchanged (${BEFORE})"; else res failure_restore_into_existing_database_refused FAIL "rc=${RC} ${BEFORE}/${AFTER} $OUTP"; fi
# F5 insufficient disk: both scripts refuse up front (a real ENOSPC mid-restore is NOT simulated: documented)
OUTP="$(BACKUP_MIN_FREE_MB=999999999 "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label nodisk 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'insufficient disk space' && ! ls "$BKUP"/*nodisk >/dev/null 2>&1; then res failure_insufficient_disk_backup_refused PASS "refused before writing anything: $(printf '%s' "$OUTP" | cut -c1-120)"; else res failure_insufficient_disk_backup_refused FAIL "rc=${RC} $OUTP"; fi
OUTP="$(RESTORE_MIN_FREE_MB=999999999 restore_run "$PLAIN_DIR" f5_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'insufficient disk space' && [ "$(db_exists f5_db)" = 0 ]; then res failure_insufficient_disk_restore_refused PASS "refused, no database created"; else res failure_insufficient_disk_restore_refused FAIL "rc=${RC} $OUTP"; fi
# F7 partial restore rolled back (fault injected: the pg_restore client is terminated part-way through the load)
OUTP="$(RESTORE_TEST_FAULT=kill_midway restore_run "$PLAIN_DIR" f7_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ "$(db_exists f7_db)" = 0 ] && printf '%s' "$OUTP" | grep -q 'dropped'; then res failure_partial_restore_rolled_back PASS "pg_restore terminated part-way (injected fault: client killed once the new database had grown), exit ${RC}; the single transaction rolled back and the half-created database was dropped"; else res failure_partial_restore_rolled_back FAIL "rc=${RC} exists=$(db_exists f7_db) $OUTP"; fi
# F8 backup credential expired / missing / wrong role / wrong target
EXPIRED='2020-01-01 00:00:00+00'
prov_backup_role src "$SRC_PORT" "$EXPIRED"
OUTP="$("${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label expired 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'credential EXPIRED\|cannot connect' && ! ls "$BKUP"/*expired >/dev/null 2>&1; then res failure_backup_credential_expired_fails_closed PASS "exit ${RC}: $(printf '%s' "$OUTP" | cut -c1-200)"; else res failure_backup_credential_expired_fails_closed FAIL "rc=${RC} $OUTP"; fi
prov_backup_role src "$SRC_PORT" "$(date -u -d '+1 hour' '+%Y-%m-%d %H:%M:%S+00')"   # re-mint (new password, new passfile)
BPF="${SECRETS}/pgpass.backup"; export BACKUP_PGPASSFILE="$BPF"
OUTP="$(BACKUP_PGPASSFILE=${SECRETS}/does-not-exist "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label nocred 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'credential file missing'; then res failure_backup_credential_missing_fails_closed PASS "exit ${RC}: $(printf '%s' "$OUTP" | cut -c1-140)"; else res failure_backup_credential_missing_fails_closed FAIL "rc=${RC} $OUTP"; fi
OUTP="$(BACKUP_PGUSER=igaming BACKUP_PGPASSFILE="${SECRETS}/pgpass" "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label asowner 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'does not have BYPASSRLS' && ! ls "$BKUP"/*asowner >/dev/null 2>&1; then res failure_backup_as_owner_role_refused PASS "refused before dumping: $(printf '%s' "$OUTP" | cut -c1-150)"; else res failure_backup_as_owner_role_refused FAIL "rc=${RC} $OUTP"; fi
OUTP="$(BACKUP_PGPORT=5432 "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label shared 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'SHARED local cluster'; then res guard_refuses_shared_cluster_5432 PASS "refused without connecting"; else res guard_refuses_shared_cluster_5432 FAIL "rc=${RC} $OUTP"; fi
OUTP="$(BACKUP_PGHOST=db.example.invalid "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label remote 2>&1)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'BACKUP_ALLOW_REMOTE_TARGET'; then res guard_refuses_remote_host_without_explicit_allow PASS "refused without connecting"; else res guard_refuses_remote_host_without_explicit_allow FAIL "rc=${RC} $OUTP"; fi
# M5: host aliases of the SHARED cluster are canonicalised and refused (the full list is a unit test in backup_static_test.go)
ALIAS_OK=0; ALIAS_N=0
for h in 127.0.0.2 127.1 0.0.0.0 ::ffff:127.0.0.1 LOCALHOST localhost. '[::1]' 'a,127.0.0.1' "$(hostname)"; do
  ALIAS_N=$((ALIAS_N + 1))
  OUTP="$(BACKUP_PGHOST="$h" BACKUP_PGPORT=5432 "${SCRIPT_DIR}/backup.sh" --out-dir "$BKUP" --label alias 2>&1)" && RC=0 || RC=$?
  if [ "$RC" -ne 0 ] && ! printf '%s' "$OUTP" | grep -q 'cannot connect'; then ALIAS_OK=$((ALIAS_OK + 1)); else echo "alias ${h}: rc=${RC} ${OUTP:0:120}" >>"${WORK}/alias.fail"; fi
done
if [ "$ALIAS_OK" = "$ALIAS_N" ]; then res guard_refuses_aliases_of_the_shared_cluster PASS "${ALIAS_N}/${ALIAS_N} aliases of 127.0.0.1:5432 refused by the guard BEFORE any connection (127.0.0.2, 127.1, 0.0.0.0, ::ffff:127.0.0.1, LOCALHOST, localhost., [::1], a,127.0.0.1, this host's own name)"; else res guard_refuses_aliases_of_the_shared_cluster FAIL "$(tr '\n' ';' <"${WORK}/alias.fail")"; fi

# ---- M3: the owner role is allow-listed -----------------------------------------------------------------------------------
M3_OK=0; M3_N=0
for orole in postgres igaming_backup igaming_runtime rdsadmin rds_superuser app_owner; do
  M3_N=$((M3_N + 1))
  OUTP="$(restore_run "$PLAIN_DIR" m3_db --owner-role "$orole")" && RC=0 || RC=$?
  if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -qE 'is refused|RESTORE_OWNER_ROLE_OVERRIDE' && [ "$(db_exists m3_db)" = 0 ]; then M3_OK=$((M3_OK + 1)); else echo "owner ${orole}: rc=${RC} ${OUTP:0:150}" >>"${WORK}/m3.fail"; fi
done
if [ "$M3_OK" = "$M3_N" ]; then res restore_owner_role_allow_list PASS "--owner-role postgres, igaming_backup, igaming_runtime, rdsadmin, rds_superuser refused outright; an unlisted name (app_owner) needs the explicit RESTORE_OWNER_ROLE_OVERRIDE; no database created in any case"; else res restore_owner_role_allow_list FAIL "$(tr '\n' ';' <"${WORK}/m3.fail")"; fi

# ---- H3: signing is REQUIRED; tampering, unsigned sets and attacker-signed replacement sets are refused ---------------------
auth_count() { grep -c 'connection authorized: user=drill_restore' "${DRILL_ROOT}/src/server.log" || true; }
h3_case() { # name dir regex [VAR=value | -VAR (unset)]...
  local name="$1" dir="$2" rx="$3" c0 c1 out rc kv; shift 3
  c0="$(auth_count)"
  # shellcheck disable=SC2163 # intentional: export the KEY=VALUE string held in kv (a -KEY argument is unset instead)
  out="$( ( for kv in "$@"; do case "$kv" in -*) unset "${kv#-}" ;; *) export -- "$kv" ;; esac; done; restore_run "$dir" h3_db ) )" && rc=0 || rc=$?
  c1="$(auth_count)"
  if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qE "$rx" && [ "$(db_exists h3_db)" = 0 ] && [ "$c0" = "$c1" ]; then
    res "$name" PASS "refused (exit ${rc}): $(printf '%s' "$out" | head -1 | cut -c1-150); no database contact (0 new connections), nothing decrypted, nothing restored"
  else res "$name" FAIL "rc=${rc} conns=${c0}->${c1} out=${out:0:300}"; fi
}
# unsigned: an ENCRYPTED set with no identity supplied - had the decrypt been reached the message would be about the identity instead
rm -rf "${BKUP}/h3"; cp -a "$ENC_DIR" "${BKUP}/h3"; rm -f "${BKUP}/h3/SHA256SUMS.sig"
h3_case restore_refuses_unsigned_set "${BKUP}/h3" 'UNSIGNED backup set' -RESTORE_AGE_IDENTITY_FILE
# attacker-signed replacement set: the attacker can encrypt to the PUBLIC recipient and sign with his own key, but not with the pinned one
rm -rf "${BKUP}/h3"; cp -a "$ENC_DIR" "${BKUP}/h3"
printf 'ALTER ROLE igaming WITH SUPERUSER;\n' | age -r "$AGE_PUB" >"${BKUP}/h3/globals.sql.age"; resign "${BKUP}/h3" "${SECRETS}/attacker.pem"
h3_case restore_refuses_attacker_signed_replacement_set "${BKUP}/h3" 'SIGNATURE VERIFICATION FAILED' -RESTORE_AGE_IDENTITY_FILE
# the attacker supplies his own public key inside the backup directory / points the pin elsewhere
cp "${SECRETS}/sign.pub" "${BKUP}/h3/sign.pub"
h3_case restore_refuses_verification_key_inside_backup_dir "${BKUP}/h3" 'must not be read from the backup directory' RESTORE_VERIFY_PUBKEY_FILE="${BKUP}/h3/sign.pub"
h3_case restore_refuses_verification_key_not_matching_pin "${BKUP}/h3" 'does not equal the pinned' RESTORE_VERIFY_PUBKEY_FILE="${SECRETS}/attacker.pub"
h3_case restore_requires_pinned_fingerprint "${BKUP}/h3" 'are required' -RESTORE_VERIFY_PUBKEY_SHA256
# altered after signing: a changed manifest.json (e.g. the environment) and a swapped checksum line
rm -rf "${BKUP}/h3"; cp -a "$PLAIN_DIR" "${BKUP}/h3"; sed -i 's/"environment_anchor": "development"/"environment_anchor": "production"/' "${BKUP}/h3/manifest.json"
h3_case restore_refuses_manifest_altered_after_signing "${BKUP}/h3" 'CHECKSUM MISMATCH'
rm -rf "${BKUP}/h3"; cp -a "$PLAIN_DIR" "${BKUP}/h3"; printf '\n' >>"${BKUP}/h3/SHA256SUMS"
h3_case restore_refuses_checksum_list_altered_after_signing "${BKUP}/h3" 'SIGNATURE VERIFICATION FAILED'
head -c 20 /dev/urandom >"${BKUP}/h3/SHA256SUMS.sig"; cp "$PLAIN_DIR/SHA256SUMS" "${BKUP}/h3/SHA256SUMS"
h3_case restore_refuses_garbage_signature "${BKUP}/h3" 'SIGNATURE VERIFICATION FAILED'
rm -rf "${BKUP}/h3"
# backup refuses to run without a signing key / with an unusable one (nothing dumped)
expect_refused backup_requires_signing_key 'BACKUP_SIGNING_KEY_FILE is required' env -u BACKUP_SIGNING_KEY_FILE "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label nosign
printf 'not a key\n' >"${SECRETS}/badkey.pem"; chmod 600 "${SECRETS}/badkey.pem"
expect_refused backup_refuses_unusable_signing_key 'cannot sign' env BACKUP_SIGNING_KEY_FILE="${SECRETS}/badkey.pem" "${SCRIPT_DIR}/backup.sh" --out-dir "$H1OUT" --label badkey

# ---- superuser data load: a planted archive entry must fail harmlessly ---------------------------------------------------
# A hand-built set (valid signature, as from a compromised backup pipeline) whose archive carries an expression index whose function
# runs COPY ... TO PROGRAM when the rows are loaded during the restore.
FLAG="${DRILL_ROOT}/src/PWNED.flag"; rm -f "$FLAG"
build_trap_archive "${WORK}/mal.dump" "COPY (SELECT 1) TO PROGRAM 'touch ${FLAG}'"
rm -rf "${BKUP}/mal"; cp -a "$PLAIN_DIR" "${BKUP}/mal"; cp "${WORK}/mal.dump" "${BKUP}/mal/db.dump"; rm -f "${WORK}/mal.dump"; rehash_manifest "${BKUP}/mal"; resign "${BKUP}/mal"
OUTP="$(restore_run "${BKUP}/mal" mal_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ ! -e "$FLAG" ] && [ "$(db_exists mal_db)" = 0 ] && printf '%s' "$OUTP" | grep -qi 'external program\|permission denied'; then
  res malicious_archive_entry_fails_harmlessly_as_nonsuperuser PASS "COPY ... TO PROGRAM planted in a CHECK-constraint function: the non-superuser restore role could not run it ($(printf '%s' "$OUTP" | grep -m1 -io 'permission denied[^.;]*\|[^ ]* external program[^.;]*' | head -1 | cut -c1-90)); single transaction rolled back, flag file NOT created, no database left"
else res malicious_archive_entry_fails_harmlessly_as_nonsuperuser FAIL "rc=${RC} flag=$([ -e "$FLAG" ] && echo created || echo absent) $OUTP"; fi
# the same restore as a SUPERUSER is refused outright
prov_set_password src "$SRC_PORT" postgres pgpass.super
OUTP="$(RESTORE_USER=postgres RESTORE_PASSFILE="${SECRETS}/pgpass.super" restore_run "${BKUP}/mal" mal_db)" && RC=0 || RC=$?
if [ "$RC" -ne 0 ] && [ ! -e "$FLAG" ] && [ "$(db_exists mal_db)" = 0 ] && printf '%s' "$OUTP" | grep -q 'refusing to run the archive as postgres'; then res restore_refuses_superuser_for_data_load PASS "pg_restore as the cluster superuser is refused before any database is created: $(printf '%s' "$OUTP" | head -1 | cut -c1-140)"; else res restore_refuses_superuser_for_data_load FAIL "rc=${RC} $OUTP"; fi
# negative control: the NAIVE procedure (pg_restore by hand as the superuser, no --role) runs the same archive's planted command with superuser rights
cl_super src "$SRC_PORT" -d postgres -c "CREATE DATABASE mal_ctl_db TEMPLATE template0" >/dev/null
PGPASSFILE="${SECRETS}/pgpass.super" pg_restore -h 127.0.0.1 -p "$SRC_PORT" -U postgres --single-transaction --exit-on-error --no-owner -d mal_ctl_db "${BKUP}/mal/db.dump" >"${WORK}/mal-ctl.out" 2>&1 && RC=0 || RC=$?
if [ -e "$FLAG" ]; then res negative_control_naive_superuser_restore_executes_planted_command PASS "control: the same archive restored the naive way (pg_restore as the superuser) RAN the planted COPY ... TO PROGRAM (flag file created by the server): exactly what the two-phase, non-superuser load prevents"; else res negative_control_naive_superuser_restore_executes_planted_command FAIL "rc=${RC} the attack did not fire, so the non-superuser test proves nothing: $(head -3 "${WORK}/mal-ctl.out" | tr '\n' ' ')"; fi
cl_super src "$SRC_PORT" -d postgres -c "DROP DATABASE IF EXISTS mal_ctl_db WITH (FORCE)" >/dev/null
rm -f "$FLAG"; cl_super src "$SRC_PORT" -d postgres -c "DROP DATABASE IF EXISTS mal_db WITH (FORCE)" >/dev/null
rm -rf "${BKUP}/mal"

# ---- SR12R-1: an UNLISTED plaintext file beside a signed encrypted set must not shadow the signed artefact -------------------------------
# PoC of the re-review: a storage writer adds a plaintext db.dump (an attacker archive whose function changes the owner's password) and baseline.txt next to a
# genuine signed set. The signature and the listed checksums still verify; the old restore then loaded the attacker's archive.
build_trap_archive "${WORK}/evil.dump" "EXECUTE format('ALTER ROLE igaming %s %L', 'PASSWORD', 'x' || 'y-poc-pw-12345')"
rm -rf "${BKUP}/sr1"; cp -a "$ENC_DIR" "${BKUP}/sr1"; cp "${WORK}/evil.dump" "${BKUP}/sr1/db.dump"; cp "${PLAIN_DIR}/baseline.txt" "${BKUP}/sr1/baseline.txt"; cp "${PLAIN_DIR}/globals.sql" "${BKUP}/sr1/globals.sql"
h3_case restore_refuses_unlisted_plaintext_beside_signed_encrypted_set "${BKUP}/sr1" "UNLISTED file" RESTORE_AGE_IDENTITY_FILE="${SECRETS}/age.key"
if psql_as igaming "${SECRETS}/pgpass" "$SRC_PORT" "$DB" -c "SELECT 1" >/dev/null 2>&1; then res unlisted_plaintext_poc_did_not_change_owner_credential PASS "the planted ALTER ROLE ... PASSWORD never ran: igaming still logs in with its own credential"; else res unlisted_plaintext_poc_did_not_change_owner_credential FAIL "igaming credential changed"; fi
# symlink / sub-directory entries
rm -rf "${BKUP}/sr1"; cp -a "$ENC_DIR" "${BKUP}/sr1"; ln -s /etc/hostname "${BKUP}/sr1/globals.sql"
h3_case restore_refuses_symlink_in_backup_dir "${BKUP}/sr1" "not a regular file" RESTORE_AGE_IDENTITY_FILE="${SECRETS}/age.key"
rm -rf "${BKUP}/sr1"; cp -a "$ENC_DIR" "${BKUP}/sr1"; mkdir "${BKUP}/sr1/sub"
h3_case restore_refuses_subdirectory_in_backup_dir "${BKUP}/sr1" "not a regular file" RESTORE_AGE_IDENTITY_FILE="${SECRETS}/age.key"
# a plaintext name where the signed set is encrypted (re-sealed list that mixes forms) is refused too
rm -rf "${BKUP}/sr1"; cp -a "$ENC_DIR" "${BKUP}/sr1"; cp "${PLAIN_DIR}/db.dump" "${BKUP}/sr1/db.dump"; ( cd "${BKUP}/sr1" && sha256sum -- db.dump db.dump.age globals.sql.age baseline.txt.age manifest.json >SHA256SUMS ) && openssl pkeyutl -sign -rawin -inkey "${SECRETS}/sign.pem" -in "${BKUP}/sr1/SHA256SUMS" -out "${BKUP}/sr1/SHA256SUMS.sig" 2>/dev/null
h3_case restore_refuses_signed_list_with_two_forms_of_the_dump "${BKUP}/sr1" "exactly one form of db.dump" RESTORE_AGE_IDENTITY_FILE="${SECRETS}/age.key"
# the signed manifest binds the plaintext bytes and the environment
rm -rf "${BKUP}/sr1"; cp -a "$PLAIN_DIR" "${BKUP}/sr1"; printf '\xde\xad' | dd of="${BKUP}/sr1/db.dump" bs=1 seek=100 conv=notrunc status=none; resign "${BKUP}/sr1"
h3_case restore_refuses_dump_not_matching_signed_manifest_hash "${BKUP}/sr1" "plaintext_dump_sha256"
rm -rf "${BKUP}/sr1"; cp -a "$PLAIN_DIR" "${BKUP}/sr1"; sed -i 's/"environment_anchor": "development"/"environment_anchor": "production"/' "${BKUP}/sr1/manifest.json"; resign "${BKUP}/sr1"
h3_case restore_refuses_manifest_anchor_differing_from_baseline "${BKUP}/sr1" "differs from the signed manifest" RESTORE_EXPECT_SOURCE_ENVIRONMENT=production RESTORE_TARGET_ENVIRONMENT=production
rm -rf "${BKUP}/sr1"
h3_case restore_refuses_set_not_matching_pinned_set_hash "$PLAIN_DIR" "does not equal RESTORE_EXPECT_SET_SHA256" RESTORE_EXPECT_SET_SHA256=0000000000000000000000000000000000000000000000000000000000000000
h3_case restore_refuses_other_target_environment "$PLAIN_DIR" "only allowed between the same environment" RESTORE_TARGET_ENVIRONMENT=production
h3_case restore_requires_target_environment "$PLAIN_DIR" "RESTORE_TARGET_ENVIRONMENT" -RESTORE_TARGET_ENVIRONMENT
# SR12R2-1: an artefact turned into a SYMLINK after the scan must not be followed after verification: the copy is by content and checksummed
rm -rf "${BKUP}/sr1"; cp -a "$PLAIN_DIR" "${BKUP}/sr1"
c0="$(auth_count)"
OUTP="$(RESTORE_TEST_FAULT=symlink_after_scan restore_run "${BKUP}/sr1" symlink_db)" && RC=0 || RC=$?
c1="$(auth_count)"
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'CHECKSUM MISMATCH' && [ "$c1" = "$c0" ] && [ "$(db_exists symlink_db)" = 0 ]; then res restore_refuses_artefact_swapped_to_symlink_after_scan PASS "manifest.json replaced by a symlink to attacker content after the scan: copied by content, checksum mismatch, refused with 0 database connections (a link in the private copy would have been re-read from the attacker's file)"; else res restore_refuses_artefact_swapped_to_symlink_after_scan FAIL "rc=${RC} $OUTP"; fi
rm -rf "${BKUP}/sr1"
# TOCTOU: the originals are overwritten AFTER verification; only the verified private copies may be used
rm -rf "${BKUP}/sr1"; cp -a "$PLAIN_DIR" "${BKUP}/sr1"
OUTP="$(RESTORE_TEST_FAULT=swap_after_verify restore_run "${BKUP}/sr1" toctou_db)" && RC=0 || RC=$?
TC="$(psql_as igaming_backup "$BPF" "$SRC_PORT" toctou_db -c 'SELECT count(*) FROM ledger_entries' 2>/dev/null || echo -1)"
if [ "$RC" -eq 0 ] && [ "$TC" = "$ENTRIES" ] && [ "$(head -c 4 "${BKUP}/sr1/db.dump")" = EVIL ]; then res restore_uses_only_verified_private_copies PASS "originals overwritten after verification (db.dump now starts with EVIL), the restore still loaded the signed content: ledger_entries=${TC}"; else res restore_uses_only_verified_private_copies FAIL "rc=${RC} count=${TC} $OUTP"; fi
psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c "DROP DATABASE IF EXISTS toctou_db WITH (FORCE)" >/dev/null 2>&1 || true
rm -rf "${BKUP}/sr1" "${WORK}/evil.dump"

# ---- SR12R-2: privileged roles are refused as owner and as loading identity; an archive that alters roles is detected ----------------------
cl_super src "$SRC_PORT" -d postgres -c "GRANT pg_execute_server_program TO igaming" >/dev/null
OUTP="$(restore_run "$PLAIN_DIR" sr2_db)" && RC=0 || RC=$?
OUTP2="$(RESTORE_USER=postgres RESTORE_PASSFILE="${SECRETS}/pgpass.super" restore_run "$PLAIN_DIR" sr2_db --phase provision)" && RC2=0 || RC2=$?
cl_super src "$SRC_PORT" -d postgres -c "REVOKE pg_execute_server_program FROM igaming" >/dev/null
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'member-of-pg_execute_server_program' && [ "$RC2" -ne 0 ] && printf '%s' "$OUTP2" | grep -q 'owner role igaming holds member-of-pg_execute_server_program' && [ "$(db_exists sr2_db)" = 0 ]; then
  res restore_refuses_owner_or_loader_in_pg_execute_server_program PASS "with igaming a member of pg_execute_server_program: the loading role (via igaming) and the owner check both refuse; no database created"
else res restore_refuses_owner_or_loader_in_pg_execute_server_program FAIL "rc=${RC}/${RC2} $OUTP $OUTP2"; fi
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE drill_restore CREATEROLE" >/dev/null
OUTP="$(restore_run "$PLAIN_DIR" sr2_db)" && RC=0 || RC=$?
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE drill_restore NOCREATEROLE" >/dev/null
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'holds CREATEROLE' && [ "$(db_exists sr2_db)" = 0 ]; then res restore_refuses_loader_with_createrole PASS "a CREATEROLE loading role is refused (a planted CREATE ROLE ... LOGIN would be a persistent backdoor)"; else res restore_refuses_loader_with_createrole FAIL "rc=${RC} $OUTP"; fi
for grp in pg_write_server_files pg_read_server_files pg_read_all_data pg_write_all_data pg_signal_backend; do
  cl_super src "$SRC_PORT" -d postgres -c "GRANT ${grp} TO drill_restore" >/dev/null
  OUTP="$(restore_run "$PLAIN_DIR" sr2_db)" && RC=0 || RC=$?
  cl_super src "$SRC_PORT" -d postgres -c "REVOKE ${grp} FROM drill_restore" >/dev/null
  if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q "member-of-${grp}"; then :; else echo "${grp}: rc=${RC} ${OUTP:0:160}" >>"${WORK}/sr2.fail"; fi
done
if [ ! -s "${WORK}/sr2.fail" ]; then res restore_refuses_loader_in_privileged_pg_roles PASS "membership in pg_write_server_files, pg_read_server_files, pg_read_all_data, pg_write_all_data, pg_signal_backend each refused for the loading role"; else res restore_refuses_loader_in_privileged_pg_roles FAIL "$(tr '\n' ';' <"${WORK}/sr2.fail")"; fi
# planted ALTER ROLE ... SET (detected by the role fingerprint) and ALTER ROLE ... PASSWORD of the owner (detected by reconnecting as the owner)
build_trap_archive "${WORK}/sr2a.dump" "ALTER ROLE igaming SET work_mem = '1GB'"
rm -rf "${BKUP}/sr2"; cp -a "$PLAIN_DIR" "${BKUP}/sr2"; cp "${WORK}/sr2a.dump" "${BKUP}/sr2/db.dump"; rehash_manifest "${BKUP}/sr2"; resign "${BKUP}/sr2"
OUTP="$(restore_run "${BKUP}/sr2" sr2_db)" && RC=0 || RC=$?
cl_super src "$SRC_PORT" -d postgres -c "ALTER ROLE igaming RESET work_mem" >/dev/null
if [ "$RC" -eq 3 ] && printf '%s' "$OUTP" | grep -q 'ROLE CHANGE DETECTED' && [ "$(db_exists sr2_db)" = 0 ]; then res planted_alter_role_set_detected_and_database_dropped PASS "an archive entry that ran ALTER ROLE igaming SET work_mem committed a cluster-global role change; the load detected it (exit 3) and dropped the database"; else res planted_alter_role_set_detected_and_database_dropped FAIL "rc=${RC} exists=$(db_exists sr2_db) $OUTP"; fi
build_trap_archive "${WORK}/sr2b.dump" "EXECUTE format('ALTER ROLE igaming %s %L', 'PASSWORD', 'x' || 'y-planted-9876')"
cp "${WORK}/sr2b.dump" "${BKUP}/sr2/db.dump"; rehash_manifest "${BKUP}/sr2"; resign "${BKUP}/sr2"
cl_super src "$SRC_PORT" -d postgres -c "CREATE DATABASE sr2_pw OWNER igaming TEMPLATE template0" >/dev/null
OUTP="$(RESTORE_MAINT_DB=$DB RESTORE_USER=igaming RESTORE_PASSFILE="${SECRETS}/pgpass" restore_run "${BKUP}/sr2" sr2_pw --phase load)" && RC=0 || RC=$?
OWNPW="$(pw_of "${SECRETS}/pgpass" "$SRC_PORT" igaming)"
IGPW="$OWNPW" cl_super src "$SRC_PORT" -d postgres <<'SQL' >/dev/null
\getenv pw IGPW
ALTER ROLE igaming WITH PASSWORD :'pw';
SQL
if [ "$RC" -eq 3 ] && printf '%s' "$OUTP" | grep -q 'CREDENTIAL CHANGE DETECTED'; then res planted_owner_password_change_detected PASS "an archive entry changed the owner's password (cluster-global, committed); reconnecting as the owner failed and the load exited 3 with an incident message (credential then restored by the drill's superuser)"; else res planted_owner_password_change_detected FAIL "rc=${RC} $OUTP"; fi
cl_super src "$SRC_PORT" -d postgres -c "DROP DATABASE IF EXISTS sr2_pw WITH (FORCE)" >/dev/null
rm -rf "${BKUP}/sr2" "${WORK}/sr2a.dump" "${WORK}/sr2b.dump" "${WORK}/sr2.fail" "${SECRETS}/pgpass.super"
psql_as igaming "${SECRETS}/pgpass" "$SRC_PORT" "$DB" -c "SELECT 1" >/dev/null 2>&1 || bk_die "owner credential not restored after the SR12R-2 tests"

# ================================================================= phase 4: full-loss restore into a NEW cluster
section "4 full-loss restore: new empty cluster -> globals -> restore -> re-provision -> verify"
psql_as drill_restore "$RPF" "$SRC_PORT" postgres -c "DROP DATABASE IF EXISTS ${RST_DB} WITH (FORCE)" >/dev/null   # free disk
T_PROV=$(now_s)
cl_init loss "$LOSS_PORT" "max_wal_size = 128MB
min_wal_size = 48MB"
cl_start loss "$LOSS_PORT"
meas "TIMING|fullloss_empty_cluster_initdb_and_start_seconds|$(elapsed_s "$T_PROV" "$(now_s)")"
LOSSPW="$(cl_random_secret)"
LPF="${SECRETS}/pgpass.loss"
IGPW="$LOSSPW" cl_super loss "$LOSS_PORT" -d postgres <<'SQL' >/dev/null
\getenv pw IGPW
ALTER ROLE postgres WITH PASSWORD :'pw';
SQL
cl_write_pass postgres "$LOSSPW" "$LOSS_PORT" "$LPF"
LOSS_DB=igaming_platform_dev
loss_admin() { RESTORE_PORT=$LOSS_PORT RESTORE_USER=postgres RESTORE_PASSFILE=$LPF restore_run "$@"; }
# a TAMPERED globals.sql, re-sealed with a VALID signature (a compromised pipeline): the role allow-list must still refuse and create nothing
cp -a "$PLAIN_DIR" "${BKUP}/tg"; sed -i 's/^ALTER ROLE igaming WITH NOSUPERUSER/ALTER ROLE igaming WITH SUPERUSER/' "${BKUP}/tg/globals.sql"; resign "${BKUP}/tg"
OUTP="$(loss_admin "${BKUP}/tg" "$LOSS_DB" --apply-globals --phase provision)" && RC=0 || RC=$?
NROLES="$(cl_super loss "$LOSS_PORT" -d postgres -c "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'igaming%'")"
NDB="$(cl_super loss "$LOSS_PORT" -d postgres -c "SELECT count(*) FROM pg_database WHERE datname = '${LOSS_DB}'")"
if [ "$RC" -ne 0 ] && printf '%s' "$OUTP" | grep -q 'tampered or unexpected role statement' && [ "$NROLES" = 0 ] && [ "$NDB" = 0 ]; then res failure_tampered_globals_refused PASS "ALTER ROLE igaming WITH SUPERUSER in a validly-signed globals.sql refused by the allow-list; no role or database created"; else res failure_tampered_globals_refused FAIL "rc=${RC} roles=${NROLES} db=${NDB} $OUTP"; fi
rm -rf "${BKUP}/tg"
# phase 1 (provision): the superuser creates only the roles (no passwords) and the EMPTY database; nothing from the archive runs
T=$(now_s)
POUT="$(loss_admin "$PLAIN_DIR" "$LOSS_DB" --apply-globals --phase provision)" && PRC=0 || PRC=$?
if [ "$PRC" -ne 0 ]; then echo "$POUT" >&2; fi
NOBJ="$(cl_super loss "$LOSS_PORT" -d "$LOSS_DB" -c "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'")"
OWN="$(cl_super loss "$LOSS_PORT" -d postgres -c "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '${LOSS_DB}'")"
if [ "$PRC" -eq 0 ] && [ "$NOBJ" = 0 ] && [ "$OWN" = igaming ]; then res fullloss_provision_phase_creates_roles_and_empty_database PASS "superuser phase created the project roles WITHOUT passwords and an empty database owned by igaming (${NOBJ} objects); no archive content executed"; else res fullloss_provision_phase_creates_roles_and_empty_database FAIL "rc=${PRC} objects=${NOBJ} owner=${OWN} $POUT"; fi
# credentials do not come back from a dump: log in must fail until they are re-provisioned
if PGPASSFILE=/dev/null psql -X -q -At -h 127.0.0.1 -p "$LOSS_PORT" -U igaming -d postgres -w -c 'select 1' >/dev/null 2>&1; then res restored_roles_have_no_password_until_reprovisioned FAIL "login worked without a password"; else res restored_roles_have_no_password_until_reprovisioned PASS "igaming/igaming_runtime/igaming_backup cannot log in after restore (no passwords in the dump, by design)"; fi
# the superuser must not run the archive even into the pre-provisioned database
OUTP="$(loss_admin "$PLAIN_DIR" "$LOSS_DB" --phase load)" && RC=0 || RC=$?
NOBJ2="$(cl_super loss "$LOSS_PORT" -d "$LOSS_DB" -c "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'")"
if [ "$RC" -ne 0 ] && [ "$NOBJ2" = 0 ] && printf '%s' "$OUTP" | grep -q 'refusing to run the archive as postgres'; then res fullloss_load_phase_refuses_superuser PASS "--phase load as the bootstrap superuser refused; the database is still empty"; else res fullloss_load_phase_refuses_superuser FAIL "rc=${RC} objects=${NOBJ2} $OUTP"; fi
# credentials come from the secret store: set the owner's and runtime's passwords, then load AS THE NON-SUPERUSER OWNER
T_RP=$(now_s)
for r in igaming igaming_runtime; do
  NPW="$(cl_random_secret)"; IGPW="$NPW" RR="$r" cl_super loss "$LOSS_PORT" -d postgres <<'SQL' >/dev/null
\getenv pw IGPW
\getenv rr RR
ALTER ROLE :"rr" WITH PASSWORD :'pw';
SQL
  cl_write_pass "$r" "$NPW" "$LOSS_PORT" "${SECRETS}/pgpass"
done
RP_SEC="$(elapsed_s "$T_RP" "$(now_s)")"
LOUT="$(RESTORE_PORT=$LOSS_PORT RESTORE_USER=igaming RESTORE_PASSFILE="${SECRETS}/pgpass" restore_run "$PLAIN_DIR" "$LOSS_DB" --phase load)" && LRC=0 || LRC=$?
if [ "$LRC" -ne 0 ]; then echo "$LOUT" >&2; fi
echo "$LOUT" | grep -E '^TIMING' | sed 's/restore_/fullloss_restore_/' | tee -a "$OUT"
meas "TIMING|fullloss_provision_and_load_phases_seconds|$(elapsed_s "$T" "$(now_s)")"
if [ "$LRC" -eq 0 ]; then res full_loss_restore_into_new_cluster PASS "two-phase restore: roles + empty database created by the bootstrap superuser WITHOUT passwords, archive then loaded as the NON-superuser igaming"; else res full_loss_restore_into_new_cluster FAIL "$(echo "$LOUT" | tail -3 | tr '\n' ' ')"; fi
T_RP=$(now_s)
prov_backup_role loss "$LOSS_PORT" "$(date -u -d '+1 hour' '+%Y-%m-%d %H:%M:%S+00')" pgpass.backup.loss "$LOSS_DB"
meas "TIMING|fullloss_reprovision_credentials_seconds|$(elapsed_s "$T_RP" "$(now_s)")"
VOUT="$(VERIFY_BACKUP_PASSFILE="${SECRETS}/pgpass.backup.loss" verify_run "$LOSS_PORT" "$LOSS_DB" "${PLAIN_DIR}/baseline.txt")" && VRC=0 || VRC=$?
echo "$VOUT" | grep -E '^(CHK|TIMING|VERIFY)' >"${WORK}/verify-fullloss.txt"; grep -E '^TIMING' "${WORK}/verify-fullloss.txt" | sed 's/verify_/fullloss_verify_/' | tee -a "$OUT"
if [ "$VRC" -eq 0 ]; then res full_loss_restore_verified PASS "$(tail -1 "${WORK}/verify-fullloss.txt"); incl. database_environment anchor BLANK => a production platform-api would REFUSE to start until the owner records it"; else res full_loss_restore_verified FAIL "$(grep FAIL "${WORK}/verify-fullloss.txt" | head -4 | cut -c1-200 | tr '\n' ' ')"; fi
# the object-level grants came with the dump; the pg_catalog function REVOKEs (harden-backup-surface.sql) did NOT: the environment script must re-run it
if psql_as igaming_backup "${SECRETS}/pgpass.backup.loss" "$LOSS_PORT" "$LOSS_DB" -c "SELECT pg_try_advisory_lock(7)" >/dev/null 2>&1; then PRE_H=open; else PRE_H=closed; fi
prov_harden loss "$LOSS_PORT"
if psql_as igaming_backup "${SECRETS}/pgpass.backup.loss" "$LOSS_PORT" "$LOSS_DB" -c "SELECT pg_try_advisory_lock(7)" >/dev/null 2>&1; then POST_H=open; else POST_H=closed; fi
if [ "$PRE_H" = open ] && [ "$POST_H" = closed ]; then res hardening_must_be_reapplied_after_full_loss_restore PASS "on the restored cluster the backup role could take advisory locks until harden-backup-surface.sql was re-run (pg_catalog grants are not part of a dump), then it could not: the runbook post-restore checklist includes the step"; else res hardening_must_be_reapplied_after_full_loss_restore FAIL "pre=${PRE_H} post=${POST_H} acl=$(cl_super loss "$LOSS_PORT" -d "$LOSS_DB" -c "select coalesce(proacl::text,'NULL') from pg_proc where proname='pg_try_advisory_lock' and pronargs=1")"; fi
# the owner records the environment TRUTHFULLY (the source was a development database): the production startup guard then refuses on the dev anchor
cl_super loss "$LOSS_PORT" -d "$LOSS_DB" -c "SET ROLE igaming; INSERT INTO database_environment (environment) VALUES ('development')" >/dev/null 2>"${WORK}/anchor.err" && ANCH=ok || ANCH="$(head -2 "${WORK}/anchor.err" | tr '\n' ' ')"
AOUT="$(PGPASSFILE="${SECRETS}/pgpass" DATABASE_URL="postgres://igaming_runtime@127.0.0.1:${LOSS_PORT}/${LOSS_DB}?sslmode=disable" "${WORK}/restorecheck" -expect-anchor development 2>&1 | grep -E 'environment_anchor_state|startup_guard_production' || true)"
if [ "$ANCH" = ok ] && printf '%s' "$AOUT" | grep -q 'environment_anchor_state|PASS' && printf '%s' "$AOUT" | grep -q 'startup_guard_production_process_refused_on_dev_or_test_anchor|PASS'; then res anchor_recorded_truthfully_production_guard_refuses_dev_data PASS "owner recorded 'development' (the source environment); the production startup guard REFUSES this database, so a restored dev backup can never be started as production by accident"; else res anchor_recorded_truthfully_production_guard_refuses_dev_data FAIL "anchor=${ANCH} ${AOUT}"; fi
# SR12R-9: the IAM-only role path leaves igaming_backup WITHOUT any password (the 15-minute IAM token is the credential on RDS)
BACKUP_ROLE_IAM_ONLY=1 cl_super loss "$LOSS_PORT" -d "$LOSS_DB" -f "${SCRIPT_DIR}/init-backup-role.sql" >/dev/null
NOPW="$(cl_super loss "$LOSS_PORT" -d postgres -c "SELECT (rolpassword IS NULL)::text || ',' || rolbypassrls::text || ',' || rolconnlimit FROM pg_authid WHERE rolname = 'igaming_backup'")"
if [ "$NOPW" = "true,true,2" ] && ! psql_as igaming_backup "${SECRETS}/pgpass.backup.loss" "$LOSS_PORT" "$LOSS_DB" -c "SELECT 1" >/dev/null 2>&1; then res init_backup_role_iam_only_path_has_no_password PASS "BACKUP_ROLE_IAM_ONLY=1: igaming_backup has a NULL password (the old password no longer works), BYPASSRLS and CONNECTION LIMIT 2 intact; rds_iam is granted only where it exists"; else res init_backup_role_iam_only_path_has_no_password FAIL "state=${NOPW}"; fi
cl_stop loss; rm -rf "$(cl_dir loss)"

# ================================================================= phase 6: PITR
if [ "$SKIP_PITR" -eq 0 ]; then
  section "6 point-in-time recovery (physical base backup + WAL archive)"
  rm -rf "${BKUP:?}"/* 2>/dev/null || true   # logical backup sets are no longer needed: free disk for the base backup
  bk_check_free 600 "PITR"
  PITR_OUT="$("${SCRIPT_DIR}/pitr-drill.sh" --drill-root "$DRILL_ROOT" --src-port "$SRC_PORT" --rec-port 5497 2>&1)" && PRC=0 || PRC=$?
  printf '%s\n' "$PITR_OUT" | grep -E '^(RESULT|TIMING|SIZE|PITR_TARGET)' | tee -a "$OUT"
  [ "$PRC" -eq 0 ] || { FAILS=$((FAILS + 1)); printf '%s\n' "$PITR_OUT" | tail -15 >&2; }
fi

# ================================================================= summary
section "summary"
NPASS="$(grep -c '^RESULT|.*|PASS|' "$OUT" || true)"; NFAIL="$(grep -c '^RESULT|.*|FAIL|' "$OUT" || true)"
echo "RESULTS: ${NPASS} PASS, ${NFAIL} FAIL"
[ -z "$RESULTS_FILE" ] || cp "$OUT" "$RESULTS_FILE"
if [ "$NFAIL" -eq 0 ] && [ "$FAILS" -eq 0 ]; then echo "A12 DRILL RESULT: PASS"; exit 0; fi
echo "A12 DRILL RESULT: FAIL"; exit 1
