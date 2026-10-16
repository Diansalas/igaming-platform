#!/usr/bin/env bash
# shellcheck disable=SC2034  # the RN_S_* secret variables are read by lib.sh and the sourced modes
# Entry point of the staging runner image (STAGING-PREP-3). ONE command argument selects the MODE; parameters are the
# non-secret container-environment overrides deploy.sh passes (deploy.sh runner <mode> KEY=VALUE...). Docs:
# docs/plans/staging-prep-3-record.md, docs/runbooks/aws-staging-deploy-plan.md.
#
#   verify-db             read-only SQL verification pack (plan 3.1-3.3, 6.1/6.2, database side of V1-V10)
#   seed-actor-proof      write the actor_proof_keys verification row (operational-runbooks.md section 15)
#   seed-market           write the synthetic operating-market rows (cmd/seed-market)
#   seed-world            create the synthetic ACTIVE tenant(s), brand(s) and tenant staff the acceptance suite needs (cmd/seed-staging-world)
#   record-anchor         record the database_environment anchor (staging|development|test), once, owner-only (plan 6.2)
#   cleanup               remove leftovers of an interrupted drill: loader role, restore-check database (idempotent)
#   backup-restore-drill  provision igaming_backup, harden, probes, ephemeral keys, backup, two-phase restore, verify, drop (plan 5, 8, 7.2)
#   pitr-verify           verify a PITR-restored instance (plan 7.4)
#   probe-backup-role     the denied-probe pack alone, re-runnable (plan 5.3)
#   usage                 print this list (the task definition's default command; does nothing else)
# Started by boot.sh (env -i allow-list, bash --noprofile --norc). FIRST, before any other program runs: no tracing, whatever the inherited shell
# options say, and the secrets move into NON-EXPORTED shell variables (RN_S_*), so no child process (mktemp, psql, awk, ...) ever inherits one.
# Normal path: boot.sh hands them over on file descriptor 3 (RN_SECRETS_FD=3), one value per line, in this fixed order; they were never in the
# environment of this shell nor in any argument list. Fallback (the file is run directly, e.g. by a test): the DB_*_PASSWORD / ACTOR_PROOF_KEYS /
# STAFF_MFA_* environment variables are copied and then unset. Nothing above this block expands a secret.
set +x +v
PS4='+ '
RN_S_MASTER="" RN_S_OWNER="" RN_S_RUNTIME="" RN_S_ACTOR_KEYS="" RN_S_MFA_FACTOR_KEYS="" RN_S_MFA_MAC_KEYS=""
if [ "${RN_SECRETS_FD-}" = 3 ]; then
  IFS= read -r -u 3 RN_S_MASTER || true
  IFS= read -r -u 3 RN_S_OWNER || true
  IFS= read -r -u 3 RN_S_RUNTIME || true
  IFS= read -r -u 3 RN_S_ACTOR_KEYS || true
  IFS= read -r -u 3 RN_S_MFA_FACTOR_KEYS || true
  IFS= read -r -u 3 RN_S_MFA_MAC_KEYS || true
  exec 3<&-
  unset RN_SECRETS_FD
else
  RN_S_MASTER="${DB_MASTER_PASSWORD-}"
  RN_S_OWNER="${DB_OWNER_PASSWORD-}"
  RN_S_RUNTIME="${DB_RUNTIME_PASSWORD-}"
  RN_S_ACTOR_KEYS="${ACTOR_PROOF_KEYS-}"
  RN_S_MFA_FACTOR_KEYS="${STAFF_MFA_FACTOR_KEYS-}"
  RN_S_MFA_MAC_KEYS="${STAFF_MFA_MAC_KEYS-}"
fi
unset DB_MASTER_PASSWORD DB_OWNER_PASSWORD DB_RUNTIME_PASSWORD ACTOR_PROOF_KEYS STAFF_MFA_FACTOR_KEYS STAFF_MFA_MAC_KEYS
set -euo pipefail

RN_SELF="$(cd "${BASH_SOURCE[0]%/*}" && pwd)"
# shellcheck source=deploy/staging-runner/lib.sh
. "${RN_SELF}/lib.sh"

MODE="${1:-usage}"
if [ "$#" -gt 1 ]; then
  printf 'RUNNER|REFUSED|exactly one command argument (the mode) is accepted; parameters are environment overrides, never arguments\n'
  printf 'RUNNER|END|exit=2|mode=%s\n' "$MODE"
  exit 2
fi

case "$MODE" in
  usage|help|-h|--help)
    sed -n '3,/^# Started by boot/p' "${BASH_SOURCE[0]}" | sed -e 's/^# \{0,1\}//' -e '/^Started by boot/d'
    printf 'RUNNER|END|exit=0|mode=usage\n'
    exit 0
    ;;
  verify-db|seed-actor-proof|seed-market|seed-world|record-anchor|cleanup|backup-restore-drill|pitr-verify|probe-backup-role) ;;
  *)
    printf 'RUNNER|REFUSED|unknown mode (see usage)\n'
    printf 'RUNNER|END|exit=2|mode=%s\n' "$(printf '%s' "$MODE" | tr -c 'A-Za-z0-9_-' '?' | cut -c1-40)"
    exit 2
    ;;
esac

# shellcheck source=/dev/null
. "${RN_SELF}/modes/${MODE}.sh"
rn_init "$MODE"
"mode_${MODE//-/_}"
rn_finish
