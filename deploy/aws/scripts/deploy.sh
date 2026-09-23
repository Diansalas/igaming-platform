#!/usr/bin/env bash
# Staging lifecycle script (Stage 9.4, ADR 0086). The narrated procedure —
# including the one-time bootstrap, acceptance, teardown and cost notes —
# is docs/runbooks/stage-9-4-staging-lifecycle-runbook.md; this script is
# its scriptable form.
#
# Commands (run from anywhere inside the repo):
#   deploy.sh up                 create the environment and deploy HEAD (or re-apply the SAME commit)
#   deploy.sh scale <N>          set platform-api's desired count (2 = multi-replica test, then back to 1)
#   deploy.sh migrate            re-run role-init + migrate, then roll the services (e.g. after a rotation)
#   deploy.sh seed-admin <email> create the first platform_admin (Back Office login); password in Secrets Manager
#   deploy.sh status             print URLs and service state (read-only)
#   deploy.sh down               terraform destroy, then run verify-teardown.sh
#
# One commit per environment lifetime: `up` refuses to deploy a DIFFERENT
# commit onto a running environment, because a full apply would put the
# new code in front of the old schema before migrations run (ADR 0086 /
# docs/architecture/38-deployment-architecture.md §2.3: migrations run
# before the new version receives traffic). To test a new commit: `down`,
# then `up`.
#
# Guarantees:
# - Image identity is the FULL git commit SHA of a CLEAN checkout (ECR tags
#   are immutable). A dirty working tree is refused — an image must be
#   reproducible from a commit.
# - Every `terraform apply`/`destroy` is interactive: this script never
#   passes -auto-approve. Review each plan before typing "yes".
# - It uses whatever AWS credentials the operator's shell already has; it
#   never reads, writes or assumes a specific credential. Terraform's
#   allowed_account_ids refuses any account other than 765578795051.
#
# Required tools: terraform (>= 1.11), docker (with buildx for
# --platform), aws CLI v2, jq, git, curl.
# Required file: deploy/aws/environments/staging/terraform.tfvars (git-
# ignored) with at least staging_access_cidrs — see terraform.tfvars.example.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
TF_DIR="${REPO_ROOT}/deploy/aws/environments/staging"
AWS_REGION="eu-central-1"
# Fargate task definitions here run on the default X86_64 platform; build
# for it explicitly so an ARM (e.g. Apple Silicon) workstation cannot
# produce an image that fails with "exec format error" on Fargate.
DOCKER_PLATFORM="linux/amd64"
PLATFORM_API_DESIRED_COUNT="${PLATFORM_API_DESIRED_COUNT:-1}"

die() { echo "ERROR: $*" >&2; exit 1; }
say() { echo; echo "-- $* --"; }

require_tools() {
  local bin
  for bin in "$@"; do
    command -v "${bin}" >/dev/null 2>&1 || die "${bin} is required but not on PATH"
  done
}

resolve_image_tag() {
  [[ -z "$(git -C "${REPO_ROOT}" status --porcelain)" ]] \
    || die "working tree is not clean — commit or stash first (images must be reproducible from a commit)"
  IMAGE_TAG="$(git -C "${REPO_ROOT}" rev-parse HEAD)"
  [[ "${IMAGE_TAG}" =~ ^[0-9a-f]{40}$ ]] || die "could not resolve a full commit SHA for HEAD"
}

# Current image tag of the deployed environment (for commands that must not
# change the image, e.g. scale). Falls back to HEAD.
deployed_image_tag() {
  (cd "${TF_DIR}" && terraform output -raw image_tag 2>/dev/null) || true
}

require_tfvars() {
  [[ -f "${TF_DIR}/terraform.tfvars" ]] \
    || die "${TF_DIR}/terraform.tfvars not found — copy terraform.tfvars.example and set staging_access_cidrs"
}

tf() { (cd "${TF_DIR}" && terraform "$@"); }

tf_apply() {
  # Interactive on purpose (no -auto-approve).
  tf apply -var "image_tag=${IMAGE_TAG}" -var "platform_api_desired_count=${PLATFORM_API_DESIRED_COUNT}" "$@"
}

tf_out() { tf output -raw "$1"; }

ecr_login() {
  local registry="$1"
  aws ecr get-login-password --region "${AWS_REGION}" | docker login --username AWS --password-stdin "${registry}"
}

image_exists() {
  local repo_url="$1" tag="$2"
  local repo_name="${repo_url#*/}"
  aws ecr describe-images --region "${AWS_REGION}" --repository-name "${repo_name}" \
    --image-ids "imageTag=${tag}" >/dev/null 2>&1
}

# build_and_push <repo_url> <expected_api_url|-> <docker build args...>
#
# Frontend images bake VITE_API_BASE_URL in at build time, so their content
# depends on the environment's API URL as well as the commit. They carry it
# as the image label igaming.api_url; an already-pushed tag is reused only
# if that label matches this environment's API URL (a half-finished
# teardown can leave old images behind while CloudFront gets a new domain).
# Pass "-" for images with no baked-in URL (platform-api).
build_and_push() {
  local repo_url="$1" expected_api_url="$2"; shift 2
  local ref="${repo_url}:${IMAGE_TAG}" label
  if image_exists "${repo_url}" "${IMAGE_TAG}"; then
    if [[ "${expected_api_url}" == "-" ]]; then
      echo "   ${ref} already exists (immutable tag) — skipping build"
      return 0
    fi
    docker pull --platform "${DOCKER_PLATFORM}" "${ref}" >/dev/null
    label="$(docker image inspect --format '{{ index .Config.Labels "igaming.api_url" }}' "${ref}")"
    [[ "${label}" == "${expected_api_url}" ]] \
      || die "${ref} exists but was built for API ${label:-<unknown>}, not ${expected_api_url} — the environment was re-created without its ECR repositories being deleted; run 'deploy.sh down' to completion, then 'up'"
    echo "   ${ref} already exists for ${expected_api_url} — skipping build"
    return 0
  fi
  local -a label_args=()
  [[ "${expected_api_url}" != "-" ]] && label_args=(--label "igaming.api_url=${expected_api_url}")
  docker build --platform "${DOCKER_PLATFORM}" -t "${ref}" ${label_args[@]+"${label_args[@]}"} "$@" "${REPO_ROOT}"
  docker push "${ref}"
}

# run_one_off_task <task_def_arn> <label> [overrides_json]
run_one_off_task() {
  local task_def="$1" label="$2" overrides="${3:-}"
  local cluster subnets sg assign network_config task_arn exit_code log_group
  local -a extra=()
  [[ -n "${overrides}" ]] && extra=(--overrides "${overrides}")
  cluster="$(tf_out ecs_cluster_name)"
  subnets="$(tf output -json ecs_task_subnet_ids)"
  sg="$(tf_out ecs_security_group_id)"
  assign="$(tf_out ecs_assign_public_ip)"
  network_config="$(jq -n --argjson subnets "${subnets}" --arg sg "${sg}" --arg assign "${assign}" \
    '{awsvpcConfiguration: {subnets: $subnets, securityGroups: [$sg], assignPublicIp: $assign}}')"

  say "one-off task: ${label}"
  task_arn="$(aws ecs run-task --region "${AWS_REGION}" --cluster "${cluster}" \
    --task-definition "${task_def}" --launch-type FARGATE \
    --network-configuration "${network_config}" ${extra[@]+"${extra[@]}"} \
    --query 'tasks[0].taskArn' --output text)"
  [[ -n "${task_arn}" && "${task_arn}" != "None" ]] || die "${label}: run-task returned no task"
  echo "   task ${task_arn} — waiting for it to stop..."
  aws ecs wait tasks-stopped --region "${AWS_REGION}" --cluster "${cluster}" --tasks "${task_arn}"
  exit_code="$(aws ecs describe-tasks --region "${AWS_REGION}" --cluster "${cluster}" --tasks "${task_arn}" \
    --query 'tasks[0].containers[0].exitCode' --output text)"
  log_group="$(tf output -json log_group_names | jq -r --arg k "${label}" '.[$k]')"
  [[ "${exit_code}" == "0" ]] \
    || die "${label} exited with code ${exit_code} — see CloudWatch log group ${log_group}"
  echo "   ${label} completed (exit 0)"
}

run_migrations() {
  # ORDER MATTERS (modules/ecs header, ADR 0084): role-init before migrate;
  # migrate chains the schema_migrations write-revoke itself.
  run_one_off_task "$(tf_out ecs_role_init_task_definition_arn)" "role-init"
  run_one_off_task "$(tf_out ecs_migrate_task_definition_arn)" "migrate"
}

roll_services() {
  local cluster svc services
  cluster="$(tf_out ecs_cluster_name)"
  services="$(tf output -json ecs_service_names | jq -r '.[]')" || die "could not read ecs_service_names"
  [[ -n "${services}" ]] || die "no ECS services in Terraform outputs"
  say "forcing a new deployment of the 3 services and waiting until stable"
  for svc in ${services}; do
    aws ecs update-service --region "${AWS_REGION}" --cluster "${cluster}" --service "${svc}" \
      --force-new-deployment >/dev/null
    echo "   ${svc}: new deployment requested"
  done
  # shellcheck disable=SC2086
  aws ecs wait services-stable --region "${AWS_REGION}" --cluster "${cluster}" --services ${services}
  echo "   all services stable"
}

smoke_check() {
  local api
  api="$(tf_out api_url)"
  say "smoke check via CloudFront (requires this machine's IP to be in staging_access_cidrs)"
  curl -fsS "${api}/healthz" >/dev/null && echo "   ${api}/healthz OK" || echo "   WARNING: ${api}/healthz failed"
  curl -fsS "${api}/readyz" >/dev/null && echo "   ${api}/readyz OK" || echo "   WARNING: ${api}/readyz failed"
}

cmd_up() {
  require_tools terraform docker aws jq git curl
  require_tfvars
  resolve_image_tag
  echo "== staging up: image ${IMAGE_TAG}, platform-api replicas ${PLATFORM_API_DESIRED_COUNT} =="

  tf init -input=false

  local deployed
  deployed="$(deployed_image_tag)"
  if [[ "${deployed}" =~ ^[0-9a-f]{40}$ && "${deployed}" != "${IMAGE_TAG}" ]]; then
    die "this environment is running ${deployed}; refusing to deploy ${IMAGE_TAG} onto it (new code would serve traffic before migrations run). Run 'deploy.sh down', then 'deploy.sh up'."
  fi

  # Phase 1: ECR repositories only, so the platform-api image can be
  # pushed before the ECS services that reference it exist.
  say "phase 1/5: ECR repositories (targeted apply)"
  tf_apply -target=module.ecr

  local api_repo b2c_repo backoffice_repo registry
  api_repo="$(tf output -json ecr_repository_urls | jq -r '.["platform-api"]')"
  b2c_repo="$(tf output -json ecr_repository_urls | jq -r '.b2c')"
  backoffice_repo="$(tf output -json ecr_repository_urls | jq -r '.backoffice')"
  registry="${api_repo%%/*}"
  ecr_login "${registry}"

  say "phase 2/5: platform-api image"
  build_and_push "${api_repo}" - -f "${REPO_ROOT}/deploy/docker/platform-api.Dockerfile"

  # Phase 3: everything else. CloudFront distributions/VPC origin take
  # several minutes. The frontend services start pulling images that do
  # not exist yet; ECS keeps retrying until phase 4 pushes them.
  say "phase 3/5: full environment (full apply)"
  tf_apply

  # Phase 4: frontends. Vite bakes the API base URL in at build time, so
  # they can only be built once the API's CloudFront URL exists. Their
  # repositories are destroyed with the environment (force_delete), so an
  # image's baked-in URL always matches the environment it runs in.
  local api_url
  api_url="$(tf_out api_url)"
  say "phase 4/5: frontend images (VITE_API_BASE_URL=${api_url})"
  # VITE_BRAND_SLUG is NOT cosmetic — b2c/src/config/brand.ts throws at
  # startup in a production build if unset; "demo-casino" is this repo's
  # seeded demo tenant (see deploy/docker/README.md).
  build_and_push "${b2c_repo}" "${api_url}" -f "${REPO_ROOT}/deploy/docker/frontend.Dockerfile" \
    --build-arg APP_DIR=b2c \
    --build-arg "VITE_API_BASE_URL=${api_url}" \
    --build-arg "VITE_BRAND_SLUG=${VITE_BRAND_SLUG:-demo-casino}" \
    --build-arg "VITE_BRAND_DISPLAY_NAME=${VITE_BRAND_DISPLAY_NAME:-Demo Casino}" \
    --build-arg "VITE_BRAND_PRIMARY_COLOR=${VITE_BRAND_PRIMARY_COLOR:-#2563eb}" \
    --build-arg "VITE_BRAND_PRIMARY_COLOR_HOVER=${VITE_BRAND_PRIMARY_COLOR_HOVER:-#1d4ed8}" \
    --build-arg "VITE_BRAND_DEFAULT_ASSET=${VITE_BRAND_DEFAULT_ASSET:-USD}"
  build_and_push "${backoffice_repo}" "${api_url}" -f "${REPO_ROOT}/deploy/docker/frontend.Dockerfile" \
    --build-arg APP_DIR=backoffice \
    --build-arg "VITE_API_BASE_URL=${api_url}"

  say "phase 5/5: database roles + migrations, then roll services"
  run_migrations
  roll_services
  smoke_check

  cmd_status
}

cmd_scale() {
  local n="${1:-}"
  [[ "${n}" =~ ^[1-4]$ ]] || die "usage: deploy.sh scale <1-4>"
  require_tools terraform aws jq
  require_tfvars
  IMAGE_TAG="$(deployed_image_tag)"
  [[ "${IMAGE_TAG}" =~ ^[0-9a-f]{40}$ ]] || die "no deployed image_tag in state — run 'deploy.sh up' first"
  PLATFORM_API_DESIRED_COUNT="${n}"
  say "platform-api desired count -> ${n} (image unchanged: ${IMAGE_TAG})"
  tf_apply
  local cluster
  cluster="$(tf_out ecs_cluster_name)"
  aws ecs wait services-stable --region "${AWS_REGION}" --cluster "${cluster}" \
    --services "$(tf output -json ecs_service_names | jq -r '.platform_api')"
  echo "   platform-api stable at ${n} task(s)"
}

cmd_migrate() {
  require_tools terraform aws jq
  run_migrations
  # Services must pick up any rotated PGPASSWORD/JWT secret only AFTER
  # role-init has applied the new runtime password in Postgres.
  roll_services
}

cmd_seed_admin() {
  local email="${1:-}"
  [[ "${email}" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || die "usage: deploy.sh seed-admin <email>"
  require_tools terraform aws jq
  local overrides secret_arn
  # The email is not a secret, so it travels as a command override; the
  # password never does (SEED_ADMIN_PASSWORD comes from Secrets Manager).
  overrides="$(jq -n --arg email "${email}" \
    '{containerOverrides: [{name: "seed-admin", command: ["/app/seed-admin", "-email", $email, "-create-person"]}]}')"
  run_one_off_task "$(tf_out ecs_seed_admin_task_definition_arn)" "seed-admin" "${overrides}"
  secret_arn="$(tf output -json secret_arns | jq -r '.seed_admin_password')"
  echo "   platform_admin ${email} created. Its password is in Secrets Manager (not printed here):"
  echo "     aws secretsmanager get-secret-value --region ${AWS_REGION} --secret-id '${secret_arn}' --query SecretString --output text"
}

cmd_status() {
  require_tools terraform aws jq
  say "status"
  echo "   image_tag:      $(tf_out image_tag)"
  echo "   B2C:            $(tf_out staging_url)"
  echo "   Back Office:    $(tf_out backoffice_url)"
  echo "   API:            $(tf_out api_url)"
  local cluster
  cluster="$(tf_out ecs_cluster_name)"
  # shellcheck disable=SC2046
  aws ecs describe-services --region "${AWS_REGION}" --cluster "${cluster}" \
    --services $(tf output -json ecs_service_names | jq -r '.[]') \
    --query 'services[].{service:serviceName,desired:desiredCount,running:runningCount,pending:pendingCount}' \
    --output table
}

cmd_down() {
  require_tools terraform aws jq
  require_tfvars
  IMAGE_TAG="$(deployed_image_tag)"
  [[ "${IMAGE_TAG}" =~ ^[0-9a-f]{40}$ ]] || IMAGE_TAG="$(git -C "${REPO_ROOT}" rev-parse HEAD)"
  say "terraform destroy (interactive — review the plan, then type yes)"
  # CloudFront distributions are disabled then deleted, and the VPC origin
  # must finish deleting before its subnets can go: expect ~15-30 minutes.
  tf destroy -var "image_tag=${IMAGE_TAG}"
  "${SCRIPT_DIR}/verify-teardown.sh"
}

case "${1:-}" in
  up) cmd_up ;;
  scale) shift; cmd_scale "$@" ;;
  migrate) cmd_migrate ;;
  seed-admin) shift; cmd_seed_admin "$@" ;;
  status) cmd_status ;;
  down) cmd_down ;;
  *)
    sed -n '2,24p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
