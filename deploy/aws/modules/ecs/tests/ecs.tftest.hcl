# Pins ADR 0086 ECS invariants: no credential ever inside DATABASE_URL,
# passwords only via PGPASSWORD secret injection, service vs one-off
# execution-role separation, capacity strategies and task networking.

mock_provider "aws" {
  mock_resource "aws_ecs_cluster" {
    defaults = {
      arn = "arn:aws:ecs:eu-central-1:765578795051:cluster/igaming-staging-cluster"
    }
  }
}

variables {
  name_prefix                = "igaming-staging"
  aws_region                 = "eu-central-1"
  task_subnet_ids            = ["subnet-0aaaaaaaaaaaaaaa1", "subnet-0aaaaaaaaaaaaaaa2"]
  ecs_security_group_id      = "sg-0aaaaaaaaaaaaaaa1"
  service_execution_role_arn = "arn:aws:iam::765578795051:role/igaming-staging-ecs-service-execution"
  one_off_execution_role_arn = "arn:aws:iam::765578795051:role/igaming-staging-ecs-one-off-execution"
  task_role_arn              = "arn:aws:iam::765578795051:role/igaming-staging-ecs-task-role"
  platform_api_image         = "765578795051.dkr.ecr.eu-central-1.amazonaws.com/igaming-staging/platform-api:0123456789abcdef0123456789abcdef01234567"
  b2c_image                  = "765578795051.dkr.ecr.eu-central-1.amazonaws.com/igaming-staging/b2c:0123456789abcdef0123456789abcdef01234567"
  backoffice_image           = "765578795051.dkr.ecr.eu-central-1.amazonaws.com/igaming-staging/backoffice:0123456789abcdef0123456789abcdef01234567"
  cors_allowed_origins       = "https://dapp.cloudfront.net,https://dadmin.cloudfront.net"
  target_group_arns = {
    platform_api = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:targetgroup/igaming-staging-api-tg/0123456789abcdef"
    b2c          = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:targetgroup/igaming-staging-b2c-tg/0123456789abcdef"
    backoffice   = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:targetgroup/igaming-staging-backoffice-tg/0123456789abcdef"
  }
  database_host          = "igaming-staging-db.abc123.eu-central-1.rds.amazonaws.com"
  database_port          = 5432
  database_name          = "igaming_platform_staging"
  db_runtime_username    = "igaming_runtime"
  db_master_username     = "igaming"
  db_master_secret_arn   = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:rds!db-mock-AbCdEf"
  db_runtime_secret_arn  = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/db-runtime-AbCdEf"
  jwt_signing_secret_arn = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/jwt-signing-secret-AbCdEf"
}

run "database_url_never_carries_a_password" {
  command = plan

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.platform_api, aws_ecs_task_definition.migrate, aws_ecs_task_definition.role_init] :
      alltrue([
        for e in lookup(jsondecode(td.container_definitions)[0], "environment", []) :
        can(regex("^postgres://[A-Za-z0-9_]+@[^/:@]+:[0-9]+/[A-Za-z0-9_]+\\?sslmode=require$", e.value)) if e.name == "DATABASE_URL"
      ])
    ])
    error_message = "DATABASE_URL must be postgres://<user>@host:port/db?sslmode=require — never a password component."
  }

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.platform_api, aws_ecs_task_definition.migrate, aws_ecs_task_definition.role_init] :
      !contains([for s in lookup(jsondecode(td.container_definitions)[0], "secrets", []) : s.name], "DATABASE_URL")
    ])
    error_message = "DATABASE_URL must be a plain env var, never a secret composed with a password."
  }
}

run "credentials_injected_from_the_right_secrets" {
  command = plan

  assert {
    condition = {
      for s in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].secrets : s.name => s.valueFrom
      } == {
      PGPASSWORD         = "${var.db_runtime_secret_arn}:password::"
      JWT_SIGNING_SECRET = var.jwt_signing_secret_arn
    }
    error_message = "platform-api must receive only the RUNTIME password (PGPASSWORD) and the JWT secret."
  }

  assert {
    condition = {
      for s in jsondecode(aws_ecs_task_definition.migrate.container_definitions)[0].secrets : s.name => s.valueFrom
      } == {
      PGPASSWORD = "${var.db_master_secret_arn}:password::"
    }
    error_message = "migrate must receive only the RDS master password."
  }

  assert {
    condition = {
      for s in jsondecode(aws_ecs_task_definition.role_init.container_definitions)[0].secrets : s.name => s.valueFrom
      } == {
      PGPASSWORD               = "${var.db_master_secret_arn}:password::"
      IGAMING_RUNTIME_PASSWORD = "${var.db_runtime_secret_arn}:password::"
    }
    error_message = "role-init must receive the master password and the runtime password to set."
  }

  assert {
    condition = anytrue([
      for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment :
      e.name == "DATABASE_URL" && startswith(e.value, "postgres://igaming_runtime@")
    ])
    error_message = "platform-api must connect as the non-owning igaming_runtime role."
  }
}

run "execution_role_separation" {
  command = plan

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.platform_api, aws_ecs_task_definition.b2c, aws_ecs_task_definition.backoffice] :
      td.execution_role_arn == var.service_execution_role_arn
    ])
    error_message = "Long-running services must use the service execution role."
  }

  assert {
    condition     = aws_ecs_task_definition.migrate.execution_role_arn == var.one_off_execution_role_arn && aws_ecs_task_definition.role_init.execution_role_arn == var.one_off_execution_role_arn
    error_message = "migrate/role-init must use the one-off execution role."
  }
}

run "production_shaped_defaults" {
  command = plan

  assert {
    condition     = one([for s in aws_ecs_cluster.this.setting : s.value if s.name == "containerInsights"]) == "enabled"
    error_message = "Module default keeps Container Insights enabled."
  }

  assert {
    condition     = alltrue([for svc in [aws_ecs_service.platform_api, aws_ecs_service.b2c, aws_ecs_service.backoffice] : svc.network_configuration[0].assign_public_ip == false])
    error_message = "Module default must not assign public IPs."
  }

  assert {
    condition     = alltrue([for svc in [aws_ecs_service.platform_api, aws_ecs_service.b2c, aws_ecs_service.backoffice] : [for c in svc.capacity_provider_strategy : c.capacity_provider] == ["FARGATE"]])
    error_message = "Module default capacity must be on-demand FARGATE only."
  }

  assert {
    condition = anytrue([
      for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment :
      e.name == "TEST_SUPPORT_ENDPOINTS_ENABLED" && e.value == "false"
    ])
    error_message = "Test-support endpoints must be disabled by module default."
  }
}

run "staging_overrides" {
  command = plan

  variables {
    container_insights_enabled = false
    assign_public_ip           = true
    trusted_proxy_count        = 2
    platform_api_capacity_provider_strategy = [
      { capacity_provider = "FARGATE", base = 1, weight = 0 },
      { capacity_provider = "FARGATE_SPOT", base = 0, weight = 1 },
    ]
    b2c_capacity_provider_strategy = [{ capacity_provider = "FARGATE_SPOT", base = 0, weight = 1 }]
  }

  assert {
    condition     = one([for s in aws_ecs_cluster.this.setting : s.value if s.name == "containerInsights"]) == "disabled"
    error_message = "container_insights_enabled = false must disable Container Insights."
  }

  assert {
    condition     = aws_ecs_service.platform_api.network_configuration[0].assign_public_ip == true
    error_message = "assign_public_ip must pass through."
  }

  assert {
    condition     = one([for c in aws_ecs_service.platform_api.capacity_provider_strategy : c.base if c.capacity_provider == "FARGATE"]) == 1
    error_message = "platform-api's first task must be on on-demand FARGATE."
  }

  assert {
    condition = anytrue([
      for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment :
      e.name == "TRUSTED_PROXY_COUNT" && e.value == "2"
    ])
    error_message = "TRUSTED_PROXY_COUNT must follow trusted_proxy_count."
  }
}

run "capacity_strategy_needs_positive_weight" {
  command = plan

  variables {
    b2c_capacity_provider_strategy = [{ capacity_provider = "FARGATE", base = 1, weight = 0 }]
  }

  expect_failures = [var.b2c_capacity_provider_strategy]
}

run "production_app_env_rejected" {
  command = plan

  variables {
    app_environment = "production"
  }

  expect_failures = [var.app_environment]
}
