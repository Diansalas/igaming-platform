# Pins the ADR 0086 execution-role split: the long-running services'
# execution role can never read the RDS master credential.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "765578795051"
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
}

variables {
  name_prefix                 = "igaming-staging"
  service_ecr_repository_arns = ["arn:aws:ecr:eu-central-1:765578795051:repository/igaming-staging/platform-api"]
  service_secret_arns         = ["arn:aws:secretsmanager:eu-central-1:765578795051:secret:runtime", "arn:aws:secretsmanager:eu-central-1:765578795051:secret:jwt"]
  service_log_group_arns      = ["arn:aws:logs:eu-central-1:765578795051:log-group:/ecs/igaming-staging/platform-api:*"]
  one_off_ecr_repository_arns = ["arn:aws:ecr:eu-central-1:765578795051:repository/igaming-staging/platform-api"]
  one_off_secret_arns         = ["arn:aws:secretsmanager:eu-central-1:765578795051:secret:rds!db-master", "arn:aws:secretsmanager:eu-central-1:765578795051:secret:runtime"]
  one_off_log_group_arns      = ["arn:aws:logs:eu-central-1:765578795051:log-group:/ecs/igaming-staging/migrate:*"]
  aws_region                  = "eu-central-1"
  runner_db_resource_id       = "db-ABCDEFGHIJKLMNOPQRSTUVWXYZ"
}

run "service_role_cannot_read_master_secret" {
  command = plan

  assert {
    condition     = !contains(flatten([for s in data.aws_iam_policy_document.execution["service"].statement : s.resources]), "arn:aws:secretsmanager:eu-central-1:765578795051:secret:rds!db-master")
    error_message = "The service execution role must not be granted the RDS master secret."
  }

  assert {
    condition     = contains(flatten([for s in data.aws_iam_policy_document.execution["one_off"].statement : s.resources]), "arn:aws:secretsmanager:eu-central-1:765578795051:secret:rds!db-master")
    error_message = "The one-off execution role must be able to read the RDS master secret."
  }

  assert {
    condition     = !contains(flatten([for s in data.aws_iam_policy_document.execution["one_off"].statement : s.resources]), "arn:aws:secretsmanager:eu-central-1:765578795051:secret:jwt")
    error_message = "The one-off execution role must not be granted the JWT signing secret."
  }

  assert {
    condition     = length(aws_iam_role.execution) == 2
    error_message = "Exactly two execution roles (service, one_off)."
  }

  assert {
    # Non-empty AND every condition pins this account (alltrue([]) would be vacuous).
    condition = length(one(data.aws_iam_policy_document.ecs_tasks_assume.statement).condition) == 1 && alltrue([
      for c in one(data.aws_iam_policy_document.ecs_tasks_assume.statement).condition :
      c.variable == "aws:SourceAccount" && toset(c.values) == toset(["765578795051"])
    ])
    error_message = "The ECS trust policy must be restricted to this account (aws:SourceAccount)."
  }
}

run "every_role_carries_the_boundary" {
  command = plan

  variables {
    permissions_boundary_arn = "arn:aws:iam::765578795051:policy/igaming-staging-ecs-role-boundary"
  }

  assert {
    condition = alltrue([
      for r in concat(values(aws_iam_role.execution), [aws_iam_role.task]) :
      r.permissions_boundary == "arn:aws:iam::765578795051:policy/igaming-staging-ecs-role-boundary"
    ])
    error_message = "Every role must carry the permissions boundary the deployer policy requires."
  }
}

run "task_role_has_no_policy_by_default" {
  command = plan

  assert {
    condition     = length(aws_iam_role_policy.task) == 0
    error_message = "The task role must carry no policy unless task_secret_arns is set."
  }
}

run "task_role_reads_only_named_secrets" {
  command = plan

  variables {
    task_secret_arns = ["arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/alert-webhook-hmac-AbCdEf"]
  }

  assert {
    condition     = length(aws_iam_role_policy.task) == 1 && length(data.aws_iam_policy_document.task) == 1
    error_message = "task_secret_arns must produce exactly one task-role policy."
  }
}

run "runner_task_role_connects_as_backup_role_on_this_instance_only" {
  command = plan

  variables {
    permissions_boundary_arn = "arn:aws:iam::765578795051:policy/igaming-staging-ecs-role-boundary"
  }

  assert {
    condition = (
      length(data.aws_iam_policy_document.runner_task.statement) == 1 &&
      one(data.aws_iam_policy_document.runner_task.statement).actions == toset(["rds-db:connect"]) &&
      one(data.aws_iam_policy_document.runner_task.statement).resources == toset(["arn:aws:rds-db:eu-central-1:765578795051:dbuser:db-ABCDEFGHIJKLMNOPQRSTUVWXYZ/igaming_backup"])
    )
    error_message = "STAGING-PREP-3: the runner task role may only rds-db:connect as igaming_backup on exactly this DbiResourceId (no wildcard, no other action)."
  }

  assert {
    condition     = aws_iam_role.runner_task.permissions_boundary == "arn:aws:iam::765578795051:policy/igaming-staging-ecs-role-boundary" && aws_iam_role.runner_task.name == "igaming-staging-ecs-runner-task-role"
    error_message = "The runner task role must carry the permissions boundary."
  }
}

run "runner_task_role_rejects_a_wildcard_instance" {
  command = plan

  variables {
    runner_db_resource_id = "*"
  }

  expect_failures = [var.runner_db_resource_id]
}

run "runner_task_role_rejects_another_database_user" {
  command = plan

  variables {
    runner_db_user = "igaming"
  }

  expect_failures = [var.runner_db_user]
}

run "runner_task_role_is_not_the_shared_task_role" {
  command = plan

  assert {
    condition     = aws_iam_role.runner_task.name != aws_iam_role.task.name
    error_message = "The runner (rds-db:connect) must never share the role platform-api and the other tasks assume."
  }
}
