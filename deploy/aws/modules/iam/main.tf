# Two roles, deliberately asymmetric:
#
# - Execution role: what the ECS AGENT uses to pull images, fetch secrets,
#   and write logs on the task's behalf, BEFORE the application code runs.
#   Scoped to exactly the ECR repos, secrets, and log groups this
#   deployment creates — never a wildcard `resource = "*"`, never
#   AdministratorAccess. The one unavoidable exception is
#   `ecr:GetAuthorizationToken`, which AWS does not support scoping to a
#   specific resource ARN at all (it is an account/region-wide token
#   operation) — documented here rather than silently used.
#
# - Task role: what the APPLICATION CODE itself would assume via the
#   container's AWS SDK, if it made any AWS API calls. It does not today —
#   platform-api's only external dependencies are Postgres and (per
#   internal/observability) an OTLP endpoint, neither of which is an AWS
#   API call requiring IAM. This role is therefore intentionally empty: no
#   permissions are granted "just in case." Extend it only when a real,
#   named AWS API call is added to the application (e.g. S3 for exports),
#   not speculatively.

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "${var.name_prefix}-ecs-execution-role"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json

  tags = merge(var.tags, { Name = "${var.name_prefix}-ecs-execution-role" })
}

data "aws_iam_policy_document" "execution" {
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
    resources = var.ecr_repository_arns
  }

  statement {
    sid       = "SecretsForContainerEnv"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = var.secret_arns
  }

  statement {
    sid = "LogsForThisDeployment"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = var.log_group_arns
  }
}

resource "aws_iam_role_policy" "execution" {
  name   = "${var.name_prefix}-ecs-execution-policy"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution.json
}

resource "aws_iam_role" "task" {
  name               = "${var.name_prefix}-ecs-task-role"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  description        = "Intentionally empty/minimal: platform-api, b2c and backoffice make no AWS API calls today. Do not attach permissions speculatively."

  tags = merge(var.tags, { Name = "${var.name_prefix}-ecs-task-role" })
}
