#!/usr/bin/env bash
# Stage 9.3 staging deploy script.
#
# This script documents/automates the exact sequence an operator runs
# AFTER `terraform apply` has created the infrastructure (ECR repos, ECS
# cluster/task definitions, RDS, Secrets Manager secrets, etc.). It does
# NOT run `terraform apply` unattended without confirmation, and it does
# NOT invent AWS credentials — it uses whatever AWS CLI credentials/
# profile are already configured in the operator's own shell.
#
# Kept deliberately simple and readable rather than bulletproof, per the
# Stage 9.3 brief. Read docs/runbooks/stage-9-3-staging-deployment-runbook.md
# for the full narrated procedure and troubleshooting; this script is the
# condensed, scriptable version of the same steps.
#
# Usage:
#   cd deploy/aws/environments/staging
#   terraform init            # first time only, or after adding a module
#   terraform apply           # review the plan, then confirm
#   ../../scripts/deploy.sh   # build+push images, run migration+role-init, roll services
#
# Required tools: terraform, docker, aws CLI (v2), jq.
# Required env: AWS credentials for the confirmed staging account (e.g.
# via `aws sso login` or exported AWS_PROFILE) — this script never reads
# or assumes any specific credential variable itself.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
TF_DIR="${REPO_ROOT}/deploy/aws/environments/staging"
IMAGE_TAG="${IMAGE_TAG:-$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo latest)}"

echo "== Stage 9.3 staging deploy =="
echo "Repo root:  ${REPO_ROOT}"
echo "TF dir:     ${TF_DIR}"
echo "Image tag:  ${IMAGE_TAG}"
echo

for bin in terraform docker aws jq; do
  command -v "${bin}" >/dev/null 2>&1 || { echo "ERROR: ${bin} is required but not on PATH" >&2; exit 1; }
done

pushd "${TF_DIR}" >/dev/null

echo "-- Reading current Terraform outputs (infrastructure must already be applied) --"
ECR_URLS_JSON=$(terraform output -json ecr_repository_urls)
CLUSTER_NAME=$(terraform output -raw ecs_cluster_name)
MIGRATE_TASK_DEF=$(terraform output -raw ecs_migrate_task_definition_arn)
ROLE_INIT_TASK_DEF=$(terraform output -raw ecs_role_init_task_definition_arn)
PRIVATE_SUBNETS_JSON=$(terraform output -json private_subnet_ids)
ECS_SG=$(terraform output -raw ecs_security_group_id)
AWS_REGION=$(terraform output -raw aws_region 2>/dev/null || echo "")
if [[ -z "${AWS_REGION}" ]]; then
  AWS_REGION=$(aws configure get region)
fi
# Default the frontends' build-time API base URL to whatever this
# deployment's own api_url output resolves to (the real HTTPS hostname if
# a domain was supplied, otherwise the plain-HTTP ALB DNS name — see the
# runbook's §5 on why the no-domain case is a smoke-test-only fallback).
# Override VITE_API_BASE_URL_B2C/VITE_API_BASE_URL_BACKOFFICE explicitly
# if you need something different.
API_URL=$(terraform output -raw api_url)

PLATFORM_API_REPO=$(echo "${ECR_URLS_JSON}" | jq -r '.["platform-api"]')
B2C_REPO=$(echo "${ECR_URLS_JSON}" | jq -r '.["b2c"]')
BACKOFFICE_REPO=$(echo "${ECR_URLS_JSON}" | jq -r '.["backoffice"]')

REGISTRY_HOST=$(echo "${PLATFORM_API_REPO}" | cut -d/ -f1)

popd >/dev/null

echo "-- Logging into ECR (${REGISTRY_HOST}) --"
aws ecr get-login-password --region "${AWS_REGION}" | docker login --username AWS --password-stdin "${REGISTRY_HOST}"

echo "-- Building and pushing platform-api --"
docker build -f "${REPO_ROOT}/deploy/docker/platform-api.Dockerfile" \
  -t "${PLATFORM_API_REPO}:${IMAGE_TAG}" "${REPO_ROOT}"
docker push "${PLATFORM_API_REPO}:${IMAGE_TAG}"

# b2c/backoffice share one parameterized Dockerfile (deploy/docker/
# frontend.Dockerfile, APP_DIR selects which app to build) — see
# deploy/docker/README.md. VITE_* build args are baked into the static
# bundle at build time (Vite has no runtime env mechanism); override any
# of the *_B2C env vars below before running this script if the defaults
# aren't right for your deployment. VITE_BRAND_SLUG is NOT cosmetic —
# b2c/src/config/brand.ts throws at browser startup in a production build
# if it's unset, to stop visitors silently registering against the wrong
# tenant; "demo-casino" matches this repo's seeded demo tenant.
echo "-- Building and pushing b2c --"
docker build -f "${REPO_ROOT}/deploy/docker/frontend.Dockerfile" \
  --build-arg APP_DIR=b2c \
  --build-arg "VITE_API_BASE_URL=${VITE_API_BASE_URL_B2C:-$API_URL}" \
  --build-arg "VITE_BRAND_SLUG=${VITE_BRAND_SLUG:-demo-casino}" \
  --build-arg "VITE_BRAND_DISPLAY_NAME=${VITE_BRAND_DISPLAY_NAME:-Demo Casino}" \
  --build-arg "VITE_BRAND_PRIMARY_COLOR=${VITE_BRAND_PRIMARY_COLOR:-#2563eb}" \
  --build-arg "VITE_BRAND_PRIMARY_COLOR_HOVER=${VITE_BRAND_PRIMARY_COLOR_HOVER:-#1d4ed8}" \
  --build-arg "VITE_BRAND_DEFAULT_ASSET=${VITE_BRAND_DEFAULT_ASSET:-USD}" \
  -t "${B2C_REPO}:${IMAGE_TAG}" "${REPO_ROOT}"
docker push "${B2C_REPO}:${IMAGE_TAG}"

echo "-- Building and pushing backoffice --"
docker build -f "${REPO_ROOT}/deploy/docker/frontend.Dockerfile" \
  --build-arg APP_DIR=backoffice \
  --build-arg "VITE_API_BASE_URL=${VITE_API_BASE_URL_BACKOFFICE:-$API_URL}" \
  -t "${BACKOFFICE_REPO}:${IMAGE_TAG}" "${REPO_ROOT}"
docker push "${BACKOFFICE_REPO}:${IMAGE_TAG}"

echo
echo "-- Applying Terraform with image_tag=${IMAGE_TAG} (updates ECS task definitions) --"
pushd "${TF_DIR}" >/dev/null
terraform apply -var "image_tag=${IMAGE_TAG}"

# Re-read task def ARNs: `terraform apply` above creates new task
# definition revisions for platform-api/b2c/backoffice, and the migrate/
# role-init task definitions may also have moved if unrelated inputs
# changed.
MIGRATE_TASK_DEF=$(terraform output -raw ecs_migrate_task_definition_arn)
ROLE_INIT_TASK_DEF=$(terraform output -raw ecs_role_init_task_definition_arn)
popd >/dev/null

NETWORK_CONFIG=$(jq -n \
  --argjson subnets "${PRIVATE_SUBNETS_JSON}" \
  --arg sg "${ECS_SG}" \
  '{awsvpcConfiguration: {subnets: $subnets, securityGroups: [$sg], assignPublicIp: "DISABLED"}}')

run_one_off_task() {
  local task_def="$1"
  local label="$2"
  echo "-- Running one-off task: ${label} (${task_def}) --"
  local task_arn
  task_arn=$(aws ecs run-task \
    --cluster "${CLUSTER_NAME}" \
    --task-definition "${task_def}" \
    --launch-type FARGATE \
    --network-configuration "${NETWORK_CONFIG}" \
    --query 'tasks[0].taskArn' --output text)
  echo "   task ARN: ${task_arn}"
  echo "   waiting for it to stop..."
  aws ecs wait tasks-stopped --cluster "${CLUSTER_NAME}" --tasks "${task_arn}"
  local exit_code
  exit_code=$(aws ecs describe-tasks --cluster "${CLUSTER_NAME}" --tasks "${task_arn}" \
    --query 'tasks[0].containers[0].exitCode' --output text)
  if [[ "${exit_code}" != "0" ]]; then
    echo "ERROR: ${label} task exited with code ${exit_code}. Check CloudWatch Logs (log group for '${label}') before proceeding." >&2
    exit 1
  fi
  echo "   ${label} completed successfully (exit code 0)."
}

# ORDER MATTERS — see deploy/aws/modules/ecs's header comment and the
# runbook: role-init MUST complete before migrate, and migrate's own
# command chains the authoritative schema_migrations write-revoke after
# `cmd/migrate up` in the same task invocation.
run_one_off_task "${ROLE_INIT_TASK_DEF}" "role-init"
run_one_off_task "${MIGRATE_TASK_DEF}" "migrate"

echo
echo "-- Forcing a new deployment of the 3 long-running services --"
pushd "${TF_DIR}" >/dev/null
SERVICE_NAMES_JSON=$(terraform output -json ecs_service_names)
popd >/dev/null

for key in platform_api b2c backoffice; do
  svc_name=$(echo "${SERVICE_NAMES_JSON}" | jq -r --arg k "${key}" '.[$k]')
  aws ecs update-service --cluster "${CLUSTER_NAME}" --service "${svc_name}" \
    --force-new-deployment >/dev/null
  echo "   ${svc_name}: new deployment requested"
done

echo
echo "Done. Check the staging URL outputs (terraform output staging_url / backoffice_url / api_url)"
echo "and confirm /healthz and /readyz both return 200 before considering this deploy live."
