#!/usr/bin/env bash
# Post-restore validation of a database against the baseline captured by backup.sh (or, for PITR, against the
# baseline evaluated on the source at the recovery target). FAIL-CLOSED: a check that cannot run is a FAIL.
#
#   BACKUP_PGHOST=.. BACKUP_PGPORT=.. BACKUP_PGDATABASE=<restored db> BACKUP_PGUSER=igaming_backup BACKUP_PGPASSFILE=.. \
#   VERIFY_OWNER_PGUSER=igaming VERIFY_OWNER_PGPASSFILE=.. \
#   VERIFY_OWNER_DATABASE_URL=... VERIFY_RUNTIME_DATABASE_URL=... MIGRATE_BIN=.. RESTORECHECK_BIN=.. \
#   deploy/backup/verify.sh --baseline <plaintext baseline.txt> [--seq-mode exact|ge] [--expect-anchor unrecorded|production|...] \
#                           [--expect-actor-proof unprovisioned|provisioned|skip] [--actor-proof-kid <new kid>] [--retired-kid <old kid>]...
#
# Gates, in order (every one must PASS):
#   1. baseline.sql re-evaluated on the restored database as the BYPASSRLS backup role and compared line for line:
#      per-table row counts AND content hashes (TABLE), sequences (SEQ), roles/memberships/ACLs by name (ROLE, MEMBER,
#      DBACL, SCHEMAACL, ACL, COLACL, FUNCACL, DEFACL), triggers incl. enabled state (TRG: the append-only/immutability
#      guards), policies (POL), RLS enabled+forced flags (RLS), catalog fingerprints (FP). All INV lines must be 0:
#      ledger SUM(debits)=SUM(credits) per transaction and per tenant+asset, balance projection == recompute from
#      ledger_entries (drift 0), no sequence behind its column, FORCE RLS + a policy on every tenant table.
#   2. deploy/drill/verify.sql as the owner role: every CHK PASS (RLS forced, runtime-role pins incl. the database-level
#      TEMPORARY revoke of migration 0116, append-only grants, schema_migrations write protection).
#   3. `cmd/migrate verify` (applied-migration checksums) as the owner.
#   4. restorecheck as the runtime role: the platform's own ledger_vs_projection reconciliation per tenant (drift 0),
#      real RLS behaviour, and the database_environment startup guard behaviour (see --expect-anchor).
# Actor-proof key (owner decision 5, ADR 0122 section 10): a LOGICAL restore carries no actor-proof signing secret. --expect-actor-proof
# (default: `unprovisioned` with --anchor-mode reset, `skip` with --anchor-mode preserved, i.e. physical recovery, which keeps the live key):
#   unprovisioned  actor_proof_keys holds 0 rows, the platform's own startup probe says the configured kid is not an active key, and a proof
#                  presented to the SQL verifier is refused with AP002 (fail closed). Run this right after the restore.
#   provisioned    after deploy/backup/rekey-actor-proof.sh: exactly one ACTIVE key whose kid is --actor-proof-kid, none of the --retired-kid
#                  values exists, and the platform's startup probe reports the new kid active.
# Exit 0 only if everything passed; 1 otherwise; 2 on usage/preflight refusal.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=deploy/backup/lib-common.sh
. "${SCRIPT_DIR}/lib-common.sh"

BASELINE=""; SEQ_MODE=exact; EXPECT_ANCHOR=unrecorded; ANCHOR_MODE=reset; EXPECT_AP=""; AP_KID=""; RETIRED_KIDS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --baseline) shift; BASELINE="${1:-}" ;;
    --seq-mode) shift; SEQ_MODE="${1:-}" ;;
    --expect-anchor) shift; EXPECT_ANCHOR="${1:-}" ;;
    --anchor-mode) shift; ANCHOR_MODE="${1:-}" ;;
    --expect-actor-proof) shift; EXPECT_AP="${1:-}" ;;
    --actor-proof-kid) shift; AP_KID="${1:-}" ;;
    --retired-kid) shift; RETIRED_KIDS+=("${1:-}") ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 0 ;;
    *) bk_die "unknown argument: $1" ;;
  esac
  shift
done
[ -f "$BASELINE" ] || bk_die "--baseline must be the plaintext baseline.txt (decrypt an encrypted backup's baseline first)"
case "$SEQ_MODE" in exact|ge) ;; *) bk_die "--seq-mode must be exact or ge" ;; esac
case "$ANCHOR_MODE" in reset|preserved) ;; *) bk_die "--anchor-mode must be reset (logical restore: the anchor is blank) or preserved (physical recovery)" ;; esac
if [ -z "$EXPECT_AP" ]; then if [ "$ANCHOR_MODE" = reset ]; then EXPECT_AP=unprovisioned; else EXPECT_AP=skip; fi; fi
case "$EXPECT_AP" in unprovisioned|provisioned|skip) ;; *) bk_die "--expect-actor-proof must be unprovisioned, provisioned or skip" ;; esac
[[ "$AP_KID" =~ ^[A-Za-z0-9._-]{0,32}$ ]] || bk_die "--actor-proof-kid must match [A-Za-z0-9._-]{1,32}"
[ "$EXPECT_AP" != provisioned ] || [ -n "$AP_KID" ] || bk_die "--expect-actor-proof provisioned needs --actor-proof-kid (the NEW key id)"
for k in "${RETIRED_KIDS[@]}"; do [[ "$k" =~ ^[A-Za-z0-9._-]{1,32}$ ]] || bk_die "--retired-kid must match [A-Za-z0-9._-]{1,32}"; done
: "${VERIFY_OWNER_PGUSER:?VERIFY_OWNER_PGUSER is required}"
: "${VERIFY_OWNER_PGPASSFILE:?VERIFY_OWNER_PGPASSFILE is required}"
: "${VERIFY_OWNER_DATABASE_URL:?VERIFY_OWNER_DATABASE_URL is required (for cmd/migrate verify; NO password in it: the passfile is used)}"
: "${VERIFY_RUNTIME_DATABASE_URL:?VERIFY_RUNTIME_DATABASE_URL is required (for restorecheck; NO password in it: the passfile is used)}"
VERIFY_RUNTIME_PGPASSFILE="${VERIFY_RUNTIME_PGPASSFILE:-$VERIFY_OWNER_PGPASSFILE}"
for u in "$VERIFY_OWNER_DATABASE_URL" "$VERIFY_RUNTIME_DATABASE_URL"; do
  ui="${u#*://}"; ui="${ui%%/*}"
  case "$ui" in *@*) case "${ui%%@*}" in *:*) bk_die "VERIFY_*_DATABASE_URL must not embed a password (use VERIFY_OWNER_PGPASSFILE / VERIFY_RUNTIME_PGPASSFILE)" ;; esac ;; esac
  # also no password key in a query string or a keyword/value DSN (SR12R-8)
  lu="${u,,}"
  case "$lu" in *password=*|*password\ =*|*passwd=*) bk_die "VERIFY_*_DATABASE_URL must not embed a password (a password key was found); use the passfile" ;; esac
done
: "${MIGRATE_BIN:?MIGRATE_BIN is required}"
: "${RESTORECHECK_BIN:?RESTORECHECK_BIN is required}"
[ -x "$MIGRATE_BIN" ] && [ -x "$RESTORECHECK_BIN" ] || bk_die "MIGRATE_BIN / RESTORECHECK_BIN must be executable"
bk_need_tools psql awk diff sort
bk_connect_env
umask 077
WORK="$(mktemp -d "${TMPDIR:-/tmp}/bk-verify.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

PASSES=0; FAILS=0
chk() { # name PASS|FAIL detail
  echo "CHK|$1|$2|$3"
  if [ "$2" = PASS ]; then PASSES=$((PASSES + 1)); else FAILS=$((FAILS + 1)); fi
}

# ---- gate 1: baseline comparison (backup role, BYPASSRLS: sees every row) ----------------------------------------
T0="$(now_s)"
if ! psql -X -q -At -v ON_ERROR_STOP=1 -f "${SCRIPT_DIR}/baseline.sql" >"${WORK}/restored.txt" 2>"${WORK}/restored.err"; then
  chk baseline_queries_run FAIL "baseline.sql could not run on the restored database: $(head -2 "${WORK}/restored.err" | tr '\n' ' ')"
  echo "VERIFY RESULT: FAIL (${FAILS} failed)"; exit 1
fi
chk baseline_queries_run PASS "evaluated in $(elapsed_s "$T0" "$(now_s)") s"

if [ "$ANCHOR_MODE" = reset ]; then
  # M4: a logical restore must NOT carry the environment anchor: its data is excluded from the comparison and MUST be absent
  ANCH_ROWS="$(awk -F'|' '$1=="TABLE" && $2=="database_environment" {print $3}' "${WORK}/restored.txt")"
  if [ "$ANCH_ROWS" = 0 ]; then chk anchor_blank_after_restore PASS "database_environment holds 0 rows: a production start refuses until the owner records the environment deliberately"; else chk anchor_blank_after_restore FAIL "database_environment holds ${ANCH_ROWS:-?} row(s) after a logical restore"; fi
  grep -v -e '^TABLE|database_environment|' -e '^TABLE|actor_proof_keys|' "$BASELINE" >"${WORK}/baseline.noanchor"; BASELINE_CMP="${WORK}/baseline.noanchor"
  grep -v -e '^TABLE|database_environment|' -e '^TABLE|actor_proof_keys|' "${WORK}/restored.txt" >"${WORK}/restored.noanchor"; RESTORED_CMP="${WORK}/restored.noanchor"
  # owner decision 5: the logical dump excludes the actor-proof signing secrets by design, so the table is NOT compared with the baseline; it is asserted instead
  AP_ROWS="$(awk -F'|' '$1=="TABLE" && $2=="actor_proof_keys" {print $3}' "${WORK}/restored.txt")"
  AP_KIDS="$(grep '^META|actor_proof_kids|' "${WORK}/restored.txt" | cut -d'|' -f3- || true)"
  case "$EXPECT_AP" in
    unprovisioned)
      if [ "$AP_ROWS" = 0 ] && [ -z "$AP_KIDS" ]; then chk actor_proof_keys_empty_after_logical_restore PASS "actor_proof_keys holds 0 rows: no actor-proof secret came back through the backup; the platform fails closed until deploy/backup/rekey-actor-proof.sh provisions a NEW key"
      else chk actor_proof_keys_empty_after_logical_restore FAIL "actor_proof_keys holds ${AP_ROWS:-?} row(s) after a logical restore (kids: ${AP_KIDS:-none}): signing material travelled through the backup"; fi ;;
    provisioned)
      bad=""
      [ "$AP_KIDS" = "${AP_KID}:active" ] || bad="the table holds '${AP_KIDS:-nothing}', expected exactly '${AP_KID}:active'"
      for k in "${RETIRED_KIDS[@]}"; do case ",${AP_KIDS}," in *",${k}:"*) bad="${bad}; retired kid ${k} is present" ;; esac; done
      if [ -z "$bad" ]; then chk actor_proof_key_is_the_new_one PASS "exactly one actor-proof key, kid ${AP_KID}, active; the source's kid(s) ${RETIRED_KIDS[*]:-(none given)} are not present"; else chk actor_proof_key_is_the_new_one FAIL "$bad"; fi ;;
  esac
else
  BASELINE_CMP="$BASELINE"; RESTORED_CMP="${WORK}/restored.txt"
  # physical recovery (PITR / snapshot) lives inside the cloud trust boundary and keeps the live actor-proof key by design: it is compared like any other table
  # (row count and a hash over the NON-secret columns), and the key rows must have survived
  AP_B="$(awk -F'|' '$1=="TABLE" && $2=="actor_proof_keys" {print $3}' "$BASELINE")"; AP_R="$(awk -F'|' '$1=="TABLE" && $2=="actor_proof_keys" {print $3}' "${WORK}/restored.txt")"
  if [ "${AP_B:-0}" -ge 1 ]; then
    if [ "$AP_B" = "${AP_R:-}" ]; then chk actor_proof_keys_retained_by_physical_recovery PASS "${AP_R} actor-proof key row(s) recovered (physical recovery keeps the live key; only logical dumps exclude it)"; else chk actor_proof_keys_retained_by_physical_recovery FAIL "baseline ${AP_B} rows, recovered ${AP_R:-?}"; fi
  fi
fi

for kind in TABLE ROLE MEMBER DBACL SCHEMAACL ACL COLACL FUNCACL DEFACL TRG POL RLS FP; do
  grep "^${kind}|" "$BASELINE_CMP" | sort >"${WORK}/b.${kind}" || true
  grep "^${kind}|" "$RESTORED_CMP" | sort >"${WORK}/r.${kind}" || true
  # MEMBER may legitimately be empty (no platform role is a member of another role: that is the point); every other kind must exist
  if [ ! -s "${WORK}/b.${kind}" ] && [ "$kind" != MEMBER ]; then chk "equal_${kind}" FAIL "baseline has no ${kind} lines (baseline unusable)"; continue; fi
  if diff -q "${WORK}/b.${kind}" "${WORK}/r.${kind}" >/dev/null; then
    chk "equal_${kind}" PASS "$(wc -l <"${WORK}/b.${kind}") lines identical to the baseline"
  else
    chk "equal_${kind}" FAIL "$(diff "${WORK}/b.${kind}" "${WORK}/r.${kind}" | grep -c '^[<>]') differing lines, first: $(diff "${WORK}/b.${kind}" "${WORK}/r.${kind}" | grep '^[<>]' | head -3 | tr '\n' ' ')"
  fi
done
grep '^SEQ|' "$BASELINE" | sort >"${WORK}/b.SEQ" || true; grep '^SEQ|' "${WORK}/restored.txt" | sort >"${WORK}/r.SEQ" || true
if [ "$SEQ_MODE" = exact ]; then
  if diff -q "${WORK}/b.SEQ" "${WORK}/r.SEQ" >/dev/null; then chk equal_SEQ PASS "$(wc -l <"${WORK}/b.SEQ") sequences identical"; else chk equal_SEQ FAIL "sequence values differ: $(diff "${WORK}/b.SEQ" "${WORK}/r.SEQ" | head -3 | tr '\n' ' ')"; fi
else
  BAD="$(join -t'|' -j1 <(awk -F'|' '{print $2"|"$3}' "${WORK}/b.SEQ") <(awk -F'|' '{print $2"|"$3}' "${WORK}/r.SEQ") | awk -F'|' '$2 != "null" && ($3 == "null" || $3+0 < $2+0) {n++} END {print n+0}')"
  N_SEQ="$(wc -l <"${WORK}/b.SEQ")"; N_R="$(wc -l <"${WORK}/r.SEQ")"
  if [ "$BAD" = 0 ] && [ "$N_SEQ" = "$N_R" ]; then chk sequences_not_behind_baseline PASS "${N_SEQ} sequences; none behind the baseline (PostgreSQL pre-logs sequence values, so equal-or-greater is expected after WAL replay)"
  else chk sequences_not_behind_baseline FAIL "behind=${BAD} count baseline=${N_SEQ} restored=${N_R}"; fi
fi
INV_NZ="$(awk -F'|' '$1=="INV" && $3 != "0"' "${WORK}/restored.txt" | tr '\n' ' ')"
INV_N="$(grep -c '^INV|' "${WORK}/restored.txt" || true)"
if [ -z "$INV_NZ" ] && [ "$INV_N" -ge 7 ]; then chk invariants_all_zero PASS "${INV_N} invariant queries, 0 violations (ledger debit=credit per tx and per tenant+asset, projection==recompute, sequences, FORCE RLS+policy, triggers enabled)"
else chk invariants_all_zero FAIL "violations or missing invariant lines (${INV_N} evaluated): ${INV_NZ}"; fi
BASE_INV_NZ="$(awk -F'|' '$1=="INV" && $3 != "0"' "$BASELINE" | tr '\n' ' ')"
if [ -z "$BASE_INV_NZ" ]; then chk baseline_itself_invariant_clean PASS "the backup's own snapshot had 0 invariant violations"; else chk baseline_itself_invariant_clean FAIL "source snapshot already violated: ${BASE_INV_NZ}"; fi

# ---- gate 2: the platform drill verifier as the owner role -------------------------------------------------------
if PGUSER="$VERIFY_OWNER_PGUSER" PGPASSFILE="$VERIFY_OWNER_PGPASSFILE" psql -X -At -v ON_ERROR_STOP=1 -f "${REPO_ROOT}/deploy/drill/verify.sql" >"${WORK}/owner-verify.txt" 2>"${WORK}/owner-verify.err"; then
  NCHK="$(grep -c '^CHK|' "${WORK}/owner-verify.txt" || true)"
  NBAD="$(grep '^CHK|' "${WORK}/owner-verify.txt" | awk -F'|' '$3 != "PASS"' | cut -d'|' -f2 | tr '\n' ' ')"
  if [ "$NCHK" -gt 0 ] && [ -z "$NBAD" ]; then chk drill_verify_sql_owner PASS "${NCHK} CHK lines PASS (RLS forced, runtime-role pins incl. TEMPORARY revoke 0116, append-only grants)"
  else chk drill_verify_sql_owner FAIL "checks=${NCHK} failing: ${NBAD}"; fi
else
  chk drill_verify_sql_owner FAIL "verify.sql did not run: $(head -2 "${WORK}/owner-verify.err" | tr '\n' ' ')"
fi

# ---- gate 3: applied migration checksums -------------------------------------------------------------------------
if OUT="$(PGPASSFILE="$VERIFY_OWNER_PGPASSFILE" DATABASE_URL="$VERIFY_OWNER_DATABASE_URL" "$MIGRATE_BIN" -dir "${REPO_ROOT}/migrations" verify 2>&1)"; then
  chk migrate_verify PASS "$(printf '%s' "$OUT" | tail -1)"
else
  chk migrate_verify FAIL "$(printf '%s' "$OUT" | tail -2 | tr '\n' ' ')"
fi

# ---- gate 4: the application's own code against the restored database --------------------------------------------
RC_ARGS=(-expect-anchor "$EXPECT_ANCHOR" -actor-proof "$EXPECT_AP")
[ -z "$AP_KID" ] || RC_ARGS+=(-actor-proof-kid "$AP_KID")
for k in "${RETIRED_KIDS[@]}"; do RC_ARGS+=(-retired-kid "$k"); done
if OUT="$(PGPASSFILE="$VERIFY_RUNTIME_PGPASSFILE" DATABASE_URL="$VERIFY_RUNTIME_DATABASE_URL" "$RESTORECHECK_BIN" "${RC_ARGS[@]}" 2>&1)"; then RC=0; else RC=$?; fi
printf '%s\n' "$OUT" | grep '^CHK|' | while IFS= read -r line; do echo "$line"; done
NPASS="$(printf '%s\n' "$OUT" | grep -c '^CHK|.*|PASS|' || true)"; NFAIL="$(printf '%s\n' "$OUT" | grep -c '^CHK|.*|FAIL|' || true)"
PASSES=$((PASSES + NPASS)); FAILS=$((FAILS + NFAIL))
if [ "$RC" -ne 0 ] && [ "$NFAIL" -eq 0 ]; then chk restorecheck_ran FAIL "restorecheck exit ${RC}: $(printf '%s' "$OUT" | tail -2 | tr '\n' ' ')"; fi

echo "TIMING|verify_total_seconds|$(elapsed_s "$T0" "$(now_s)")"
if [ "$FAILS" -eq 0 ]; then echo "VERIFY RESULT: PASS (${PASSES} checks)"; exit 0; fi
echo "VERIFY RESULT: FAIL (${FAILS} failed, ${PASSES} passed)"
exit 1
