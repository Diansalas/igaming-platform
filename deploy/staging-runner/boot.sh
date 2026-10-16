#!/bin/sh
# First stage of the staging runner image (STAGING-PREP-3, SRP3-6). POSIX sh, no bash features: nothing here can be steered by
# SHELLOPTS / BASHOPTS / BASH_ENV / ENV / PS4 / BASH_XTRACEFD / IFS, which a `run-task --overrides` could set in the container
# environment to make the next bash trace (`set -x`) and print every expanded line, including the passfile line with the password.
#
# It re-executes the real entrypoint under `env -i` with an EXPLICIT allow-list of variable names: everything else in the inherited
# environment (including every shell-behaviour variable) is dropped, and the new bash is started with --noprofile --norc.
#
# The SECRETS (DB_*_PASSWORD, ACTOR_PROOF_KEYS, STAFF_MFA_FACTOR_KEYS / STAFF_MFA_MAC_KEYS) are deliberately NOT in the allow-list: they would
# have to be written as NAME=value arguments of `env`, i.e. into a command line. They travel instead on file descriptor 3, a here-document
# (an in-memory pipe or an unlinked file, never a named path), one value per line in a fixed order, and entrypoint.sh reads them with the
# `read` builtin into non-exported shell variables before it runs any other program. The exec'd bash therefore has no secret in its
# environment, and no child process ever inherits one.
#
# Honest limit: whatever starts this script as PID 1 (with initProcessEnabled, tini) keeps the initial environment block, secrets included,
# in /proc/<pid>/environ for the life of the task, readable only by processes of the same uid; there is no ECS Exec and the task has no
# inbound path. That is the same exposure as every ECS-injected secret.
set +x +v
set -eu

here="$(cd "$(dirname "$0")" && pwd)"

# Allow-list of NON-secret variables: the database target, the ECS credential-endpoint variables the AWS CLI needs, and the runner PARAMETERS
# (the names deploy.sh accepts per mode). Names that are not set are not passed.
names="APP_ENV RDS_HOST RDS_PORT RDS_CA_BUNDLE_URL DB_NAME DB_MASTER_USER DB_OWNER_USER DB_RUNTIME_USER ACTOR_PROOF_ACTIVE_KID
STAFF_MFA_FACTOR_ACTIVE_KID STAFF_MFA_MAC_ACTIVE_KID STAFF_MFA_ISSUER
MOCK_GEOLOCATION_COUNTRY AWS_REGION AWS_DEFAULT_REGION AWS_CONTAINER_CREDENTIALS_RELATIVE_URI SEED_WORLD_SECRET_ARN
TENANT_SLUG BRAND_SLUG SECOND_TENANT_SLUG SECOND_BRAND_SLUG COUNTRY AUTH_REF OPERATOR REASON ACTOR_ID DRY_RUN
EXPECT_BACKUP_ROLE GRANT_LISTING REQUIRE_ANCHOR EXPECT_HARDENING EXPECT_ANCHOR PITR_ENDPOINT PITR_PORT EXPECTED_DIGESTS PITR_SHOW_LINES
DRILL_RESTORE_DB SKIP_RESTORE PRINT_EXPECTED ANCHOR_VALUE CLEANUP_DB"
# The local test harness only (lib.sh honours these together with RUNNER_ALLOW_LOCAL=local-test-harness and a loopback target on 5497-5499).
path_value="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
home_value="/home/runner"
if [ "${RUNNER_ALLOW_LOCAL-}" = "local-test-harness" ]; then
  names="${names} RN_ROOT RUNNER_ALLOW_LOCAL RN_BIN RUNNER_SCRATCH_BASE RUNNER_BACKUP_PASSWORD_FILE SEED_WORLD_LOCAL_STORE RUNNER_LOCAL_IAM_ONLY"
  path_value="${PATH}"
  home_value="${HOME:-/tmp}"
fi

# The mode argument(s) are remembered first: the positional parameters are reused to build the command.
argc=$#
a1="${1-}"
a2="${2-}"
set --
v=""
for n in $names; do
  # $n is one of the fixed names above (letters, digits, underscore); eval only reads that variable's value.
  if eval "[ \"\${$n+set}\" = set ]"; then
    eval "v=\${$n}"
    set -- "$@" "$n=$v"
  fi
done
set -- "$@" RN_SECRETS_FD=3 bash --noprofile --norc "${here}/entrypoint.sh"
case "$argc" in
  0) ;;
  1) set -- "$@" "$a1" ;;
  *) set -- "$@" "$a1" "$a2" ;;
esac

# Order is part of the contract with entrypoint.sh: master, owner, runtime, actor-proof keys, staff MFA factor keys, staff MFA MAC keys.
exec env -i PATH="$path_value" HOME="$home_value" LC_ALL=C.UTF-8 "$@" 3<<EOF
${DB_MASTER_PASSWORD-}
${DB_OWNER_PASSWORD-}
${DB_RUNTIME_PASSWORD-}
${ACTOR_PROOF_KEYS-}
${STAFF_MFA_FACTOR_KEYS-}
${STAFF_MFA_MAC_KEYS-}
EOF
