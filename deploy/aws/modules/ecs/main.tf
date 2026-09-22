# ECS Fargate cluster, log groups, task definitions and services for
# platform-api / b2c / backoffice, plus two one-off task definitions
# (migrate, role-init) that are never run as a service — they are invoked
# directly via `aws ecs run-task` (see deploy/aws/scripts/deploy.sh and
# the runbook), once per deploy, before the long-running services are
# (re)started.
#
# Sequencing these one-off tasks correctly matters and is NOT enforced by
# Terraform (run-task is an operator/pipeline action, not a Terraform
# resource) — it must be:
#   1. role-init  (creates igaming_runtime + base grants, connects as the
#      RDS master/"igaming" role)
#   2. migrate    (`cmd/migrate up`, connects as "igaming"; its own
#      container command ALSO chains the schema_migrations write-revoke
#      immediately afterward, in the same task, using the same master
#      credential — see the "migrate" task definition below and the
#      runbook for why this must be a distinct, later step, not folded
#      into step 1)
#   3. force a new deployment of the 3 long-running services
#
# Log groups are created here (co-located with the services that write to
# them) using names computed the SAME way the root module computes them
# for the IAM module's policy scoping — see environments/staging/main.tf's
# `local.log_group_names`. This module recomputes the identical name
# strings independently (both sides derive from name_prefix, a plain
# string, with no resource dependency), which is what lets the IAM
# execution role's policy be scoped to these exact log groups without
# creating a module dependency cycle between iam and ecs (iam must exist
# BEFORE ecs, since ecs task definitions reference iam's role ARNs).

locals {
  services = {
    "platform-api" = null
    "b2c"          = null
    "backoffice"   = null
    "migrate"      = null
    "role-init"    = null
  }
}

resource "aws_cloudwatch_log_group" "this" {
  for_each          = local.services
  name              = "/ecs/${var.name_prefix}/${each.key}"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, { Name = "${var.name_prefix}-${each.key}-logs" })
}

resource "aws_ecs_cluster" "this" {
  name = "${var.name_prefix}-cluster"

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-cluster" })
}

resource "aws_ecs_cluster_capacity_providers" "this" {
  cluster_name       = aws_ecs_cluster.this.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
  }
}

# --- platform-api ---

resource "aws_ecs_task_definition" "platform_api" {
  family                   = "${var.name_prefix}-platform-api"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.platform_api_cpu
  memory                   = var.platform_api_memory
  execution_role_arn       = var.execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "platform-api"
      image     = var.platform_api_image
      essential = true
      portMappings = [
        { containerPort = var.container_port, protocol = "tcp" }
      ]
      environment = [
        { name = "APP_ENV", value = var.app_environment },
        { name = "HTTP_ADDR", value = ":${var.container_port}" },
        { name = "TRUSTED_PROXY_COUNT", value = tostring(var.trusted_proxy_count) },
        { name = "CORS_ALLOWED_ORIGINS", value = var.cors_allowed_origins },
        { name = "OTEL_SERVICE_NAME", value = "platform-api" },
        { name = "OTEL_EXPORTER", value = "stdout" },
        # Stage 9.4: the second, independent gate app_environment alone is
        # no longer sufficient for — see variables.tf's own doc comment on
        # this variable and internal/config/config.go's Environment/
        # TestSupportEndpointsEnabled doc comments.
        { name = "TEST_SUPPORT_ENDPOINTS_ENABLED", value = tostring(var.test_support_endpoints_enabled) },
      ]
      secrets = [
        { name = "DATABASE_URL", valueFrom = var.database_url_runtime_secret_arn },
        { name = "JWT_SIGNING_SECRET", valueFrom = var.jwt_signing_secret_arn },
      ]
      # Liveness only (process is up) — never /readyz here. Probing
      # readiness for liveness would make the orchestrator kill a
      # perfectly healthy process during a transient DB blip it can't fix
      # by restarting. See docs/architecture/38-deployment-architecture.md §2.
      healthCheck = {
        command     = ["CMD-SHELL", "wget -q -O- http://localhost:${var.container_port}/healthz || exit 1"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 15
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["platform-api"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "platform-api"
        }
      }
    }
  ])

  tags = var.tags
}

resource "aws_ecs_service" "platform_api" {
  name            = "${var.name_prefix}-platform-api"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.platform_api.arn
  desired_count   = var.platform_api_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.ecs_security_group_id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = var.target_group_arns["platform_api"]
    container_name   = "platform-api"
    container_port   = var.container_port
  }

  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  health_check_grace_period_seconds  = 30

  tags = var.tags
}

# --- b2c ---

resource "aws_ecs_task_definition" "b2c" {
  family                   = "${var.name_prefix}-b2c"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.b2c_cpu
  memory                   = var.b2c_memory
  execution_role_arn       = var.execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "b2c"
      image     = var.b2c_image
      essential = true
      portMappings = [
        { containerPort = var.container_port, protocol = "tcp" }
      ]
      healthCheck = {
        command     = ["CMD-SHELL", "wget -q -O- http://localhost:${var.container_port}/ || exit 1"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 10
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["b2c"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "b2c"
        }
      }
    }
  ])

  tags = var.tags
}

resource "aws_ecs_service" "b2c" {
  name            = "${var.name_prefix}-b2c"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.b2c.arn
  desired_count   = var.b2c_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.ecs_security_group_id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = var.target_group_arns["b2c"]
    container_name   = "b2c"
    container_port   = var.container_port
  }

  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  health_check_grace_period_seconds  = 30

  tags = var.tags
}

# --- backoffice ---

resource "aws_ecs_task_definition" "backoffice" {
  family                   = "${var.name_prefix}-backoffice"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.backoffice_cpu
  memory                   = var.backoffice_memory
  execution_role_arn       = var.execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "backoffice"
      image     = var.backoffice_image
      essential = true
      portMappings = [
        { containerPort = var.container_port, protocol = "tcp" }
      ]
      healthCheck = {
        command     = ["CMD-SHELL", "wget -q -O- http://localhost:${var.container_port}/ || exit 1"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 10
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["backoffice"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "backoffice"
        }
      }
    }
  ])

  tags = var.tags
}

resource "aws_ecs_service" "backoffice" {
  name            = "${var.name_prefix}-backoffice"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.backoffice.arn
  desired_count   = var.backoffice_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.ecs_security_group_id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = var.target_group_arns["backoffice"]
    container_name   = "backoffice"
    container_port   = var.container_port
  }

  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  health_check_grace_period_seconds  = 30

  tags = var.tags
}

# --- migrate (one-off; run via `aws ecs run-task`, never a service) ---
#
# Command chains `cmd/migrate up` and THEN the authoritative
# schema_migrations write-revoke in the SAME task invocation, using the
# same master ("igaming") credential, so the revoke can never be
# accidentally skipped by an operator forgetting a separate step. Both
# `migrate` and `psql` are present in the platform-api image — see
# deploy/docker/platform-api.Dockerfile.
resource "aws_ecs_task_definition" "migrate" {
  family                   = "${var.name_prefix}-migrate"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.one_off_task_cpu
  memory                   = var.one_off_task_memory
  execution_role_arn       = var.execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "migrate"
      image     = var.platform_api_image
      essential = true
      command = [
        "sh", "-c",
        "/app/migrate up && psql \"$DATABASE_URL\" -v ON_ERROR_STOP=1 -c \"REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime;\""
      ]
      secrets = [
        { name = "DATABASE_URL", valueFrom = var.database_url_migration_secret_arn },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["migrate"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "migrate"
        }
      }
    }
  ])

  tags = var.tags
}

# --- role-init (one-off; run via `aws ecs run-task`, never a service) ---
#
# Runs BEFORE the migrate task in the deployment sequence (see this file's
# header comment). Connects as the master/"igaming" role
# (DATABASE_URL secret) and creates/updates igaming_runtime using the
# password ECS injects from Secrets Manager into IGAMING_RUNTIME_PASSWORD
# — never a CLI argument, never hardcoded (deploy/aws/sql/init-runtime-role.rds.sql).
resource "aws_ecs_task_definition" "role_init" {
  family                   = "${var.name_prefix}-role-init"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.one_off_task_cpu
  memory                   = var.one_off_task_memory
  execution_role_arn       = var.execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "role-init"
      image     = var.platform_api_image
      essential = true
      command = [
        "sh", "-c",
        "psql \"$DATABASE_URL\" -v ON_ERROR_STOP=1 -f /app/sql/init-runtime-role.rds.sql"
      ]
      secrets = [
        { name = "DATABASE_URL", valueFrom = var.database_url_migration_secret_arn },
        { name = "IGAMING_RUNTIME_PASSWORD", valueFrom = "${var.db_runtime_secret_arn}:password::" },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["role-init"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "role-init"
        }
      }
    }
  ])

  tags = var.tags
}
