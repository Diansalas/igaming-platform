# Three roles, deliberately asymmetric (plus the STAGING-PREP-3 runner task role at
# the end of this file):
#
# - SERVICE execution role: what the ECS AGENT uses to pull images, fetch
#   secrets, and write logs for the three long-running services
#   (platform-api, b2c, backoffice), BEFORE the application code runs.
# - ONE-OFF execution role: the same, for the one-off tasks (migrate,
#   role-init, seed-admin).
#
#   Why two execution roles (ADR 0086): the runtime-role separation of
#   docs/security/runtime-role-separation.md is now enforced at the IAM
#   layer too, not only by which secret each task definition happens to
#   reference. The service role can read ONLY the runtime DB credential and
#   the JWT signing secret; it cannot read the RDS master (migration-owner)
#   credential at all. Only the one-off role can, and it cannot read the JWT
#   signing secret. Each role is scoped to exactly the ECR repos, secrets
#   and log groups its tasks need — never a wildcard resource, never
#   AdministratorAccess. The one unavoidable exception is
#   `ecr:GetAuthorizationToken`, which AWS does not support scoping to a
#   specific resource ARN at all (it is an account/region-wide token
#   operation) — documented here rather than silently used.
#
# - Task role: what the APPLICATION CODE itself assumes via the container's
#   AWS SDK. It has NO permissions by default. The one named exception is
#   var.task_secret_arns (STAGING-PREP-1): the platform alert webhook channel
#   reads its HMAC secret through the awssm:// secret-store backend, an
#   application-level GetSecretValue on exact secret ARNs (see the policy at the
#   end of this file). Do not grant anything "just in case"; add a permission
#   only for a real, named AWS API call. KNOWN RESIDUAL (SRSP-3): the role is
#   shared by all six task definitions, so b2c, backoffice and the one-off tasks
#   can read that secret too; a dedicated platform-api task role (the others on
#   an empty one) is required before production.

# PERMISSIONS BOUNDARY (ADR 0086, security review P1): when
# var.permissions_boundary_arn is set, every role here carries it. The
# staging deployer's IAM policy only allows creating/editing roles that
# carry that exact boundary, so the deployer cannot mint a role more
# powerful than the boundary (ECR pull, staging log streams, staging
# secret reads) — i.e. it cannot escalate to account admin through these
# roles. The boundary policy itself is created once by deploy/aws/bootstrap.

data "aws_caller_identity" "current" {}

locals {
  execution_roles = {
    service = {
      description     = "ECS execution role for the long-running services (platform-api, b2c, backoffice). Cannot read the RDS master credential."
      ecr_repo_arns   = var.service_ecr_repository_arns
      secret_arns     = var.service_secret_arns
      log_group_arns  = var.service_log_group_arns
      name_suffix     = "ecs-service-execution"
      policy_name_sfx = "ecs-service-execution-policy"
    }
    one_off = {
      description     = "ECS execution role for the one-off migrate/role-init/seed-admin tasks. The only role that can read the RDS master credential."
      ecr_repo_arns   = var.one_off_ecr_repository_arns
      secret_arns     = var.one_off_secret_arns
      log_group_arns  = var.one_off_log_group_arns
      name_suffix     = "ecs-one-off-execution"
      policy_name_sfx = "ecs-one-off-execution-policy"
    }
  }
}

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }

    # Confused-deputy guard: only ECS acting for THIS account may assume.
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

resource "aws_iam_role" "execution" {
  for_each = local.execution_roles

  name                 = "${var.name_prefix}-${each.value.name_suffix}"
  description          = each.value.description
  assume_role_policy   = data.aws_iam_policy_document.ecs_tasks_assume.json
  permissions_boundary = var.permissions_boundary_arn

  tags = merge(var.tags, { Name = "${var.name_prefix}-${each.value.name_suffix}" })
}

data "aws_iam_policy_document" "execution" {
  for_each = local.execution_roles

  statement {
    sid       = "EcrAuthToken"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"] # AWS does not support resource-level scoping for this action.
  }

  statement {
    sid = "EcrPull"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
    ]
    resources = each.value.ecr_repo_arns
  }

  # Omitted entirely for a role with no secrets to read (an IAM statement
  # with an empty resource list is invalid).
  dynamic "statement" {
    for_each = length(each.value.secret_arns) > 0 ? [1] : []
    content {
      sid       = "SecretsForContainerEnv"
      actions   = ["secretsmanager:GetSecretValue"]
      resources = each.value.secret_arns
    }
  }

  statement {
    sid = "LogsForThisDeployment"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = each.value.log_group_arns
  }
}

resource "aws_iam_role_policy" "execution" {
  for_each = local.execution_roles

  name   = "${var.name_prefix}-${each.value.policy_name_sfx}"
  role   = aws_iam_role.execution[each.key].id
  policy = data.aws_iam_policy_document.execution[each.key].json
}

resource "aws_iam_role" "task" {
  name                 = "${var.name_prefix}-ecs-task-role"
  assume_role_policy   = data.aws_iam_policy_document.ecs_tasks_assume.json
  description          = "Minimal: no permissions unless var.task_secret_arns names exact secrets (STAGING-PREP-1: the platform alert webhook HMAC secret, read by platform-api through the awssm secret store). Do not attach permissions speculatively."
  permissions_boundary = var.permissions_boundary_arn

  tags = merge(var.tags, { Name = "${var.name_prefix}-ecs-task-role" })
}

# STAGING-PREP-1: the platform alert webhook channel reads its HMAC secret at
# use time through the awssm:// secret-store backend (internal/secretstore/
# awssm, ALERT_WEBHOOK_SECRET_REF). That is an application-level AWS API call
# made with THIS role, so it needs exactly one named permission: GetSecretValue
# on the listed secret ARNs and nothing else. Omitted entirely when the list is
# empty (an IAM statement with an empty resource list is invalid). The role is
# shared by every task definition (b2c, backoffice and the one-off tasks assume
# it too but make no such call); the grant is scoped to exact ARNs and capped by
# the permissions boundary (StagingAppSecrets). Residual recorded in the staging
# plan: a dedicated platform-api task role would narrow it further.
data "aws_iam_policy_document" "task" {
  count = length(var.task_secret_arns) > 0 ? 1 : 0

  statement {
    sid       = "ReadNamedApplicationSecrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = var.task_secret_arns
  }
}

resource "aws_iam_role_policy" "task" {
  count = length(var.task_secret_arns) > 0 ? 1 : 0

  name   = "${var.name_prefix}-ecs-task-policy"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task[0].json
}

# STAGING-PREP-3: the staging-runner TASK ROLE (a dedicated role, NOT the shared
# task role above: sharing it would let platform-api, b2c and every one-off task
# mint a token for the all-tenant BYPASSRLS backup login).
#
# Its ONLY permission is rds-db:connect as the backup role on THIS instance, by
# DbiResourceId: the single statement ADR 0122 section 3 requires of the "runner
# identity" (aws-staging-deploy-plan.md 4.2). A PITR clone has a different
# DbiResourceId and is deliberately NOT covered: the pitr-verify mode resets the
# backup role's credential on the disposable clone instead. No secret read, no
# rds:*, no logs, no ECR (those are the execution role's job). Capped by the
# permissions boundary, which therefore MUST allow rds-db:connect for the backup
# user (deploy/aws/bootstrap/main.tf, applied by the bootstrap credential).
resource "aws_iam_role" "runner_task" {
  name                 = "${var.name_prefix}-ecs-runner-task-role"
  assume_role_policy   = data.aws_iam_policy_document.ecs_tasks_assume.json
  description          = "staging-runner task role: rds-db:connect as ${var.runner_db_user} on this instance only. Do not attach anything else."
  permissions_boundary = var.permissions_boundary_arn

  tags = merge(var.tags, { Name = "${var.name_prefix}-ecs-runner-task-role" })
}

data "aws_iam_policy_document" "runner_task" {
  statement {
    sid       = "ConnectAsBackupRoleOnThisInstanceOnly"
    actions   = ["rds-db:connect"]
    resources = ["arn:aws:rds-db:${var.aws_region}:${data.aws_caller_identity.current.account_id}:dbuser:${var.runner_db_resource_id}/${var.runner_db_user}"]
  }
}

resource "aws_iam_role_policy" "runner_task" {
  name   = "${var.name_prefix}-ecs-runner-task-policy"
  role   = aws_iam_role.runner_task.id
  policy = data.aws_iam_policy_document.runner_task.json
}

# STAGING-PREP-3 round 2 (SA-1): the seed-world TASK ROLE, a third dedicated role. The seed-world runner mode
# generates the one-time passwords of the synthetic acceptance staff and stores them in ONE secret
# (igaming-staging/seed-world-passwords), so its only permissions are Get/PutSecretValue on that exact ARN: no
# rds-db:connect (the mode connects with the master password the execution role injects), no other secret, no
# CreateSecret/DeleteSecret/Rotate, no wildcard. Kept apart from runner_task so the all-tenant backup login and
# the secret store are never reachable from the same task. Capped by the permissions boundary, which carries the
# matching statement (deploy/aws/bootstrap/main.tf, applied by the bootstrap credential).
resource "aws_iam_role" "runner_seed_world" {
  name                 = "${var.name_prefix}-ecs-seed-world-task-role"
  assume_role_policy   = data.aws_iam_policy_document.ecs_tasks_assume.json
  description          = "seed-world runner task role: Get/PutSecretValue on the seed-world-passwords secret only. Do not attach anything else."
  permissions_boundary = var.permissions_boundary_arn

  tags = merge(var.tags, { Name = "${var.name_prefix}-ecs-seed-world-task-role" })
}

data "aws_iam_policy_document" "runner_seed_world" {
  statement {
    sid       = "ReadAndWriteTheSeedWorldSecretOnly"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:PutSecretValue"]
    resources = [var.seed_world_secret_arn]
  }
}

resource "aws_iam_role_policy" "runner_seed_world" {
  name   = "${var.name_prefix}-ecs-seed-world-task-policy"
  role   = aws_iam_role.runner_seed_world.id
  policy = data.aws_iam_policy_document.runner_seed_world.json
}
