#!/usr/bin/env bash
# Entry point of the staging runner image (STAGING-PREP-3). ONE command argument selects the MODE; parameters are the
# non-secret container-environment overrides deploy.sh passes (deploy.sh runner <mode> KEY=VALUE...). Docs:
# docs/plans/staging-prep-3-record.md, docs/runbooks/aws-staging-deploy-plan.md.
#
#   verify-db             read-only SQL verification pack (plan 3.1-3.3, 6.1/6.2, database side of V1-V10)
#   seed-actor-proof      write the actor_proof_keys verification row (operational-runbooks.md section 15)
#   seed-market           write the synthetic operating-market rows (cmd/seed-market)
#   backup-restore-drill  provision igaming_backup, harden, probes, ephemeral keys, backup, two-phase restore, verify, drop (plan 5, 8, 7.2)
#   pitr-verify           verify a PITR-restored instance (plan 7.4)
#   probe-backup-role     the denied-probe pack alone, re-runnable (plan 5.3)
#   usage                 print this list (the task definition's default command; does nothing else)
set -euo pipefail

RN_SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
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
    sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'
    printf 'RUNNER|END|exit=0|mode=usage\n'
    exit 0
    ;;
  verify-db|seed-actor-proof|seed-market|backup-restore-drill|pitr-verify|probe-backup-role) ;;
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
