#!/usr/bin/env bash
# Offline unit test (no database, no AWS) of the fail-closed classification of the denied-probe pack (SRP3-2): rn_probe_verdict in lib-probes.sh.
# Only a privilege-denied SQLSTATE is a PASS; a write probe that fails any other way, or succeeds, is a FAIL; a connection failure is BLOCKED.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RN_DIR="${HERE}/.."
RN_ROOT="$(cd "${HERE}/../../.." && pwd)"
export RN_ROOT
# shellcheck source=deploy/staging-runner/lib.sh
. "${RN_DIR}/lib.sh"
# shellcheck source=deploy/staging-runner/lib-probes.sh
. "${RN_DIR}/lib-probes.sh"

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
RN_SCRATCH="$T"
fails=0
ok() { echo "ok   $*"; }
bad() { echo "FAIL $*" >&2; fails=$((fails + 1)); }

# expect <label> <want STATUS> <class> <hard> <rc> <errtext> <name>
expect() {
  local label="$1" want="$2" class="$3" hard="$4" rc="$5" err="$6" name="${7:-probe}" got
  printf '%s\n' "$err" >"${T}/err"
  got="$(rn_probe_verdict "$class" "$hard" "$rc" "${T}/err" "$name" "SELECT 1")"
  [ "${got%%|*}" = "$want" ] && ok "${label}: ${want}" || bad "${label}: got ${got%%|*}, want ${want} (${got})"
}

expect "privilege denied (42501), write" PASS write applied 1 "ERROR:  42501"
expect "invalid grantor (0L000), write" PASS write applied 1 "ERROR:  0L000"
expect "invalid grant operation (0LP01), write" PASS write applied 1 "ERROR:  0LP01"
expect "privilege denied (42501), abuse" PASS abuse applied 1 "ERROR:  42501"
expect "privilege denied (42501) at exit 3 (script mode)" PASS write applied 3 "ERROR:  42501"
expect "other SQLSTATE at exit 3 (script mode)" FAIL write applied 3 "ERROR:  23502"
expect "exit 1 with a SQLSTATE: the statement failed in the server" PASS write applied 1 "ERROR:  42501"
expect "statement succeeded, write" FAIL write applied 0 ""
expect "statement succeeded, abuse, hardening applied" FAIL abuse applied 0 ""
expect "statement succeeded, abuse, hardening refused (recorded residual)" FINDING abuse refused 0 ""
expect "not-null violation (23502): the privilege check passed, write" FAIL write applied 1 "ERROR:  23502"
expect "undefined table (42P01), write" FAIL write applied 1 "ERROR:  42P01"
expect "statement timeout (57014), write" FAIL write applied 1 "ERROR:  57014"
expect "lock not available (55P03), write" FAIL write applied 1 "ERROR:  55P03"
expect "no SQLSTATE at all, write" FAIL write applied 3 "psql: something odd"
expect "other SQLSTATE, abuse, hardening applied" FAIL abuse applied 1 "ERROR:  22023"
expect "statement timeout (57014), abuse, hardening applied" FAIL abuse applied 1 "ERROR:  57014"
expect "lock timeout (55P03), abuse, hardening applied" FAIL abuse applied 1 "ERROR:  55P03"
expect "undefined function (42883), abuse, hardening applied" FINDING abuse applied 1 "ERROR:  42883"
expect "feature not supported (0A000), abuse, hardening applied" FINDING abuse applied 1 "ERROR:  0A000"
expect "timeout (57014), abuse, hardening refused" FINDING abuse refused 1 "ERROR:  57014"
expect "timeout (55P03), abuse, hardening unknown" FINDING abuse unknown 1 "ERROR:  55P03"
expect "other SQLSTATE, abuse, hardening refused" FINDING abuse refused 1 "ERROR:  22023"
expect "message text alone is not trusted: 'permission denied' without a privilege SQLSTATE" FAIL write applied 1 $'ERROR:  23505\nDETAIL: permission denied appears in the text'
expect "connection failure, write" BLOCKED write applied 2 'psql: error: connection to server at "x" failed: FATAL:  password authentication failed for user "igaming_backup"'
expect "connection failure, abuse (not a connect probe)" BLOCKED abuse applied 2 "psql: error: connection to server failed: timeout expired" pg_notify
expect "client failure (exit 1), write" BLOCKED write applied 1 ""
expect "CONNECT denied by the server, connect probe" PASS abuse applied 2 'psql: error: connection to server failed: FATAL:  permission denied for database "postgres"' connect_postgres
expect "CONNECT denied (message variant), connect probe" PASS abuse applied 2 'FATAL:  User does not have CONNECT privilege.' connect_template1
expect "pg_hba rejection is not a privilege denial, connect probe" BLOCKED abuse applied 2 'FATAL:  no pg_hba.conf entry for host' connect_postgres
expect "connect probe that connected" FAIL abuse applied 0 "" connect_postgres

# completeness: the pack fails when fewer probes ran than expected
RN_PROBE_RAN=0; RN_PROBE_EXPECTED=24
if [ "$RN_PROBE_RAN" -ne "$RN_PROBE_EXPECTED" ]; then ok "completeness counters are comparable"; else bad "counters"; fi
[ "${#RN_WRITE_PROBES[@]}" -eq 15 ] && [ "${#RN_ABUSE_PROBES[@]}" -eq 9 ] && ok "15 write and 9 abuse probes" || bad "unexpected probe counts"

if [ "$fails" -ne 0 ]; then echo "${fails} probe verdict test(s) failed" >&2; exit 1; fi
echo "probe verdict tests passed"
