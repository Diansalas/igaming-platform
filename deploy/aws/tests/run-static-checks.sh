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

for mod in "${AWS_DIR}"/modules/*/; do
  mod="${mod%/}"
  compgen -G "${mod}/tests/*.tftest.hcl" >/dev/null || continue
  step "terraform test: modules/$(basename "${mod}")"
  tf_in "${mod}" init -backend=false -input=false -no-color >/dev/null
  tf_in "${mod}" test -no-color
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

echo
echo "All deploy/aws static checks passed."
