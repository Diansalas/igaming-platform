#!/usr/bin/env bash
# Offline static/regression checks for deploy/aws (ADR 0086). Needs NO AWS
# credentials and creates NO AWS resources: terraform fmt/validate run with
# -backend=false, and every `terraform test` uses mock AWS providers.
# Run locally or in CI:
#   deploy/aws/tests/run-static-checks.sh
# Env: TERRAFORM (default: terraform on PATH), NODE (default: node).
set -euo pipefail

AWS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${AWS_DIR}/../.." && pwd)"
TF="${TERRAFORM:-terraform}"
NODE_BIN="${NODE:-node}"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
# One shared provider cache (symlinked into each module's data dir) instead
# of a full ~600 MB provider copy per module, which exhausted disk space.
export TF_PLUGIN_CACHE_DIR="${WORK}/plugin-cache"
mkdir -p "${TF_PLUGIN_CACHE_DIR}"

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "== $* =="; }

step "terraform version"
"${TF}" version

step "terraform fmt -check"
"${TF}" fmt -recursive -check -diff "${AWS_DIR}"

# init/test with a throwaway data dir so no .terraform/ lands in the repo.
tf_in() {
  local dir="$1"; shift
  (cd "${dir}" && TF_DATA_DIR="${WORK}/$(echo "${dir}" | tr '/' '_')" "${TF}" "$@")
}

for root in environments/staging bootstrap; do
  step "init + validate: ${root}"
  tf_in "${AWS_DIR}/${root}" init -backend=false -input=false -no-color >/dev/null
  tf_in "${AWS_DIR}/${root}" validate -no-color
done

step "terraform test: environments/staging"
tf_in "${AWS_DIR}/environments/staging" test -no-color

# Every module is validated, tested or not (an untested module — e.g. the
# unwired dns module — must still at least be valid). Each module's data
# dir is removed right after its step.
for mod in "${AWS_DIR}"/modules/*/; do
  mod="${mod%/}"
  name="$(basename "${mod}")"
  tf_in "${mod}" init -backend=false -input=false -no-color >/dev/null
  if compgen -G "${mod}/tests/*.tftest.hcl" >/dev/null; then
    step "terraform validate + test: modules/${name}"
    tf_in "${mod}" validate -no-color
    tf_in "${mod}" test -no-color
  else
    step "terraform validate (no tests): modules/${name}"
    tf_in "${mod}" validate -no-color
  fi
  rm -rf "${WORK}/$(echo "${mod}" | tr '/' '_')"
done

step "CloudFront allowlist function unit tests"
"${NODE_BIN}" --test "${AWS_DIR}/tests/edge-allowlist.test.mjs"

step "shell syntax"
for sh in "${AWS_DIR}"/scripts/*.sh "${AWS_DIR}"/tests/*.sh; do
  bash -n "${sh}" || fail "bash -n ${sh}"
done

step "repository guards"
# No Terraform state, plans or local tfvars may ever be tracked.
tracked="$(git -C "${REPO_ROOT}" ls-files | grep -E '(\.tfstate(\..*)?$|\.tfplan$|(^|/)terraform\.tfvars$|\.auto\.tfvars$|(^|/)\.terraform/)' || true)"
[[ -z "${tracked}" ]] || fail "Terraform state/plan/tfvars tracked in git: ${tracked}"
# Stage 9.3 regressions that must not come back.
if grep -rnE --include='*.tf' 'DbiResourceId\s*=' "${AWS_DIR}/modules" "${AWS_DIR}/environments" >/dev/null; then
  fail "an alarm uses the DbiResourceId dimension (AWS/RDS metrics use DBInstanceIdentifier)"
fi
if grep -rnE --include='*.tf' 'image_tag_mutability\s*=\s*"MUTABLE"' "${AWS_DIR}" >/dev/null; then
  fail "a mutable ECR repository was reintroduced"
fi
# (A value drawn from an ephemeral resource is exempt: Terraform itself
# refuses to store an ephemeral value in any state-persisted argument, so
# it can only legally appear inside a write-only argument's payload.)
if grep -rnE --include='*.tf' '^\s*(password|secret_string)\s*=' "${AWS_DIR}/modules" "${AWS_DIR}/environments" | grep -vE '=\s*ephemeral\.' >/dev/null; then
  fail "a secret value is passed through a state-persisted argument (use RDS-managed passwords / *_wo write-only arguments)"
fi
if grep -rn --include='*.tf' 'resource "random_password"' "${AWS_DIR}" >/dev/null; then
  fail "a non-ephemeral random_password (value stored in state) was reintroduced"
fi
# role-init must never print the runtime password: set_config() returns the
# value it sets, so its SELECT must stay wrapped in \o /dev/null ... \o.
if ! awk '/^\\o \/dev\/null/{q=1;next} /^\\o$/{q=0} /set_config\(.igaming\.runtime_password., :/{ if(!q) bad=1 } END{exit bad}' "${AWS_DIR}/sql/init-runtime-role.rds.sql"; then
  fail "init-runtime-role.rds.sql: SELECT set_config(...password...) is not inside \\o /dev/null (the password would be printed to CloudWatch Logs)"
fi
if ! grep -q 'RAISE EXCEPTION .igaming_runtime role create/alter failed' "${AWS_DIR}/sql/init-runtime-role.rds.sql"; then
  fail "init-runtime-role.rds.sql: CREATE/ALTER ROLE failures must be re-raised without the statement text"
fi

echo
echo "All deploy/aws static checks passed."
