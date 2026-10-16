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
#
# DATABASE CREDENTIALS (ADR 0086): DATABASE_URL is a plain, NON-secret
# environment variable naming host/port/database/user only — it carries no
# password. The password is injected separately from Secrets Manager as
# PGPASSWORD, which both pgx (internal/db, via pgconn's standard PG*
# environment fallback) and psql read whenever the connection string
# carries no password. This is what lets Terraform avoid composing (and so
# storing in state) any connection string containing a credential, and it
# works with the RDS-managed master password, whose generated characters
# would otherwise need URL-encoding. platform-api uses the runtime
# credential; migrate/role-init use the RDS master credential, through a
# separate execution role (modules/iam) that the services do not have.
#
# CAPACITY (ADR 0086): each service takes an explicit capacity provider
# strategy. The module default is on-demand FARGATE only (production-
# shaped); the staging root opts b2c/backoffice into FARGATE_SPOT and gives
# platform-api an on-demand base task with Spot for any extra replicas.

locals {
  services = {
    "platform-api" = null
    "b2c"          = null
    "backoffice"   = null
    "migrate"      = null
    "role-init"    = null
    "seed-admin"   = null
    # STAGING-PREP-3: both runner task definitions log to ONE group; the awslogs
    # stream prefix tells them apart.
    "staging-runner" = null
  }
}

resource "aws_cloudwatch_log_group" "this" {
  for_each          = local.services
  name              = "/ecs/${var.name_prefix}/${each.key}"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, { Name = "${var.name_prefix}-${each.key}-logs" })
}

locals {
  # STAGING-PREP-1: platform-api configuration beyond the base set. Every
  # name below was verified against the code (internal/config,
  # internal/staffmfa, cmd/platform-api), not against the docs. Optional
  # settings are added ONLY when their variable is non-empty, so an unset
  # owner input leaves the binary's own fail-closed default in force.
  alert_webhook_enabled = var.alert_webhook_url != ""

  platform_api_base_environment = [
    { name = "APP_ENV", value = var.app_environment },
    { name = "DATABASE_URL", value = local.database_url_runtime },
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
    # Key ids are not secret; the key material is injected from Secrets Manager below.
    { name = "STAFF_MFA_FACTOR_ACTIVE_KID", value = var.staff_mfa_active_kid },
    { name = "STAFF_MFA_MAC_ACTIVE_KID", value = var.staff_mfa_active_kid },
    { name = "PAYOUT_INSTRUMENT_ACTIVE_KID", value = var.payout_instrument_active_kid },
    { name = "PAYOUT_INSTRUMENT_FP_ACTIVE_KID", value = var.payout_instrument_active_kid },
    { name = "ACTOR_PROOF_ACTIVE_KID", value = var.actor_proof_active_kid },
  ]

  platform_api_optional_environment = concat(
    var.mock_geolocation_country != "" ? [{ name = "MOCK_GEOLOCATION_COUNTRY", value = var.mock_geolocation_country }] : [],
    var.metrics_addr != "" ? [{ name = "METRICS_ADDR", value = var.metrics_addr }] : [],
    var.alert_retention_days > 0 ? [{ name = "ALERT_RETENTION_DAYS", value = tostring(var.alert_retention_days) }] : [],
    local.alert_webhook_enabled ? [
      { name = "ALERT_WEBHOOK_URL", value = var.alert_webhook_url },
      { name = "ALERT_WEBHOOK_SECRET_REF", value = var.alert_webhook_secret_ref },
      # The webhook secret is read through the awssm:// secret-store backend (task role).
      { name = "SECRETSTORE_BACKENDS", value = "awssm" },
      { name = "AWS_SECRETSMANAGER_REGION", value = var.aws_region },
    ] : [],
  )

  # Non-secret connection strings: no password component, ever (see file
  # header). sslmode=require matches the database module's rds.force_ssl=1.
  database_url_runtime   = "postgres://${var.db_runtime_username}@${var.database_host}:${var.database_port}/${var.database_name}?sslmode=require"
  database_url_migration = "postgres://${var.db_master_username}@${var.database_host}:${var.database_port}/${var.database_name}?sslmode=require"
}

resource "aws_ecs_cluster" "this" {
  name = "${var.name_prefix}-cluster"

  # Container Insights is billed as custom CloudWatch metrics. The module
  # default keeps it on (production-shaped); the minimal staging root turns
  # it off — nothing in staging depends on it (ADR 0086).
  setting {
    name  = "containerInsights"
    value = var.container_insights_enabled ? "enabled" : "disabled"
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
  execution_role_arn       = var.service_execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "platform-api"
      image     = var.platform_api_image
      essential = true
      portMappings = [
        { containerPort = var.container_port, protocol = "tcp" }
      ]
      environment = concat(local.platform_api_base_environment, local.platform_api_optional_environment)
      secrets = [
        { name = "PGPASSWORD", valueFrom = "${var.db_runtime_secret_arn}:password::" },
        { name = "JWT_SIGNING_SECRET", valueFrom = var.jwt_signing_secret_arn },
        { name = "STAFF_MFA_FACTOR_KEYS", valueFrom = "${var.staff_mfa_keys_secret_arn}:factor_keys::" },
        { name = "STAFF_MFA_MAC_KEYS", valueFrom = "${var.staff_mfa_keys_secret_arn}:mac_keys::" },
        { name = "PAYOUT_INSTRUMENT_KEYS", valueFrom = "${var.payout_instrument_keys_secret_arn}:master_keys::" },
        { name = "PAYOUT_INSTRUMENT_FP_KEYS", valueFrom = "${var.payout_instrument_keys_secret_arn}:fp_keys::" },
        { name = "ACTOR_PROOF_KEYS", valueFrom = var.actor_proof_keys_secret_arn },
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

  lifecycle {
    precondition {
      condition     = var.alert_webhook_url == "" || var.alert_webhook_secret_ref != ""
      error_message = "alert_webhook_url requires alert_webhook_secret_ref (config.Load refuses ALERT_WEBHOOK_URL without ALERT_WEBHOOK_SECRET_REF)."
    }
    precondition {
      condition     = var.metrics_addr == "" || !endswith(var.metrics_addr, ":${var.container_port}")
      error_message = "metrics_addr must not share the HTTP port (config.Load refuses it)."
    }
  }
}

resource "aws_ecs_service" "platform_api" {
  name            = "${var.name_prefix}-platform-api"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.platform_api.arn
  desired_count   = var.platform_api_desired_count

  dynamic "capacity_provider_strategy" {
    for_each = var.platform_api_capacity_provider_strategy
    content {
      capacity_provider = capacity_provider_strategy.value.capacity_provider
      base              = capacity_provider_strategy.value.base
      weight            = capacity_provider_strategy.value.weight
    }
  }

  network_configuration {
    subnets          = var.task_subnet_ids
    security_groups  = [var.ecs_security_group_id]
    assign_public_ip = var.assign_public_ip
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

  # The capacity providers named in the strategy must be associated with
  # the cluster BEFORE the service is created (CreateService rejects an
  # unassociated provider) and must stay associated until the service is
  # gone on destroy.
  depends_on = [aws_ecs_cluster_capacity_providers.this]
}

# --- b2c ---

resource "aws_ecs_task_definition" "b2c" {
  family                   = "${var.name_prefix}-b2c"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.b2c_cpu
  memory                   = var.b2c_memory
  execution_role_arn       = var.service_execution_role_arn
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

  dynamic "capacity_provider_strategy" {
    for_each = var.b2c_capacity_provider_strategy
    content {
      capacity_provider = capacity_provider_strategy.value.capacity_provider
      base              = capacity_provider_strategy.value.base
      weight            = capacity_provider_strategy.value.weight
    }
  }

  network_configuration {
    subnets          = var.task_subnet_ids
    security_groups  = [var.ecs_security_group_id]
    assign_public_ip = var.assign_public_ip
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

  # The capacity providers named in the strategy must be associated with
  # the cluster BEFORE the service is created (CreateService rejects an
  # unassociated provider) and must stay associated until the service is
  # gone on destroy.
  depends_on = [aws_ecs_cluster_capacity_providers.this]
}

# --- backoffice ---

resource "aws_ecs_task_definition" "backoffice" {
  family                   = "${var.name_prefix}-backoffice"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.backoffice_cpu
  memory                   = var.backoffice_memory
  execution_role_arn       = var.service_execution_role_arn
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

  dynamic "capacity_provider_strategy" {
    for_each = var.backoffice_capacity_provider_strategy
    content {
      capacity_provider = capacity_provider_strategy.value.capacity_provider
      base              = capacity_provider_strategy.value.base
      weight            = capacity_provider_strategy.value.weight
    }
  }

  network_configuration {
    subnets          = var.task_subnet_ids
    security_groups  = [var.ecs_security_group_id]
    assign_public_ip = var.assign_public_ip
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

  # The capacity providers named in the strategy must be associated with
  # the cluster BEFORE the service is created (CreateService rejects an
  # unassociated provider) and must stay associated until the service is
  # gone on destroy.
  depends_on = [aws_ecs_cluster_capacity_providers.this]
}

# --- migrate (one-off; run via `aws ecs run-task`, never a service) ---
#
# Command chains `cmd/migrate up` and THEN the authoritative
# schema_migrations write-revoke in the SAME task invocation, using the
# same master ("igaming") credential, so the revoke can never be
# accidentally skipped by an operator forgetting a separate step.
# STAGING-PREP-1 (G3) / STAGING-PREP-2 (SRSP-2): the revoke runs IMMEDIATELY
# after `up`, so a failing `migrate verify` (checksum drift, version gap) can
# never leave the runtime role able to write schema_migrations; `migrate verify`
# and `migrate status` (the head of the deployed commit) follow it, and a
# failure of either makes the task exit non-zero, which stops deploy.sh before it
# rolls the services. Both
# `migrate` and `psql` are present in the platform-api image — see
# deploy/docker/platform-api.Dockerfile.
resource "aws_ecs_task_definition" "migrate" {
  family                   = "${var.name_prefix}-migrate"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.one_off_task_cpu
  memory                   = var.one_off_task_memory
  execution_role_arn       = var.one_off_execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "migrate"
      image     = var.platform_api_image
      essential = true
      command = [
        "sh", "-c",
        "/app/migrate up && psql \"$DATABASE_URL\" -v ON_ERROR_STOP=1 -c \"REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime;\" && /app/migrate verify && /app/migrate status"
      ]
      environment = [
        { name = "DATABASE_URL", value = local.database_url_migration },
      ]
      secrets = [
        { name = "PGPASSWORD", valueFrom = "${var.db_master_secret_arn}:password::" },
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
# (DATABASE_URL + PGPASSWORD from the RDS-managed master secret) and
# creates/updates igaming_runtime using the password ECS injects from
# Secrets Manager into IGAMING_RUNTIME_PASSWORD
# — never a CLI argument, never hardcoded (deploy/aws/sql/init-runtime-role.rds.sql).
resource "aws_ecs_task_definition" "role_init" {
  family                   = "${var.name_prefix}-role-init"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.one_off_task_cpu
  memory                   = var.one_off_task_memory
  execution_role_arn       = var.one_off_execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "role-init"
      image     = var.platform_api_image
      essential = true
      command = [
        "sh", "-c",
        "psql \"$DATABASE_URL\" --single-transaction -v ON_ERROR_STOP=1 -f /app/sql/init-runtime-role.rds.sql"
      ]
      environment = [
        { name = "DATABASE_URL", value = local.database_url_migration },
      ]
      secrets = [
        { name = "PGPASSWORD", valueFrom = "${var.db_master_secret_arn}:password::" },
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

# --- seed-admin (one-off; run via `deploy.sh seed-admin <email>`) ---
#
# Creates the first platform_admin staff user (cmd/seed-admin) so the Back
# Office can be used for acceptance. Connects as the master/migration-owner
# role like migrate/role-init (same one-off execution role). The password
# comes from Secrets Manager (SEED_ADMIN_PASSWORD, write-only, never in
# state); the email is supplied at run time as a command override (it is
# not a secret). Idempotency is cmd/seed-admin's own: a second run for the
# same email fails on the unique staff email rather than duplicating.
resource "aws_ecs_task_definition" "seed_admin" {
  family                   = "${var.name_prefix}-seed-admin"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.one_off_task_cpu
  memory                   = var.one_off_task_memory
  execution_role_arn       = var.one_off_execution_role_arn
  task_role_arn            = var.task_role_arn

  container_definitions = jsonencode([
    {
      name      = "seed-admin"
      image     = var.platform_api_image
      essential = true
      # Placeholder; deploy.sh always overrides the command with the
      # operator-supplied -email. Running this definition without an
      # override fails fast (cmd/seed-admin requires -email).
      command = ["/app/seed-admin"]
      environment = [
        { name = "DATABASE_URL", value = local.database_url_migration },
        # cmd/seed-admin applies the same staff MFA rules as platform-api
        # (config.ValidateStaffMFAEnv, staffmfa.LoadKeys): APP_ENV decides
        # whether published placeholder keys are refused, and the keys are
        # required in every environment.
        { name = "APP_ENV", value = var.app_environment },
        { name = "STAFF_MFA_FACTOR_ACTIVE_KID", value = var.staff_mfa_active_kid },
        { name = "STAFF_MFA_MAC_ACTIVE_KID", value = var.staff_mfa_active_kid },
      ]
      secrets = [
        { name = "PGPASSWORD", valueFrom = "${var.db_master_secret_arn}:password::" },
        { name = "SEED_ADMIN_PASSWORD", valueFrom = var.seed_admin_password_secret_arn },
        { name = "STAFF_MFA_FACTOR_KEYS", valueFrom = "${var.staff_mfa_keys_secret_arn}:factor_keys::" },
        { name = "STAFF_MFA_MAC_KEYS", valueFrom = "${var.staff_mfa_keys_secret_arn}:mac_keys::" },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["seed-admin"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "seed-admin"
        }
      }
    }
  ])

  tags = var.tags
}

# --- staging-runner (one-off; run via `deploy.sh runner <mode> [KEY=VALUE...]`, never a service) ---
#
# STAGING-PREP-3 (plan gap G1, owner actions B and C): the RDS instance is
# private, so every in-VPC operation that is not migrate / role-init / seed-admin
# runs as ONE-OFF tasks of a purpose-built image (deploy/docker/staging-runner.
# Dockerfile: bash, psql 16, OpenSSL 3, age, jq, aws cli, the deploy/backup
# scripts, the SQL verification pack, cmd/seed-market). The MODE is the single
# command argument deploy.sh passes (verify-db, seed-market, backup-restore-drill,
# pitr-verify, probe-backup-role; seed-actor-proof on the second definition);
# parameters are NON-secret container-environment overrides. The default command
# `usage` prints the modes and does nothing else.
#
# Two definitions, so key material is injected only where it is needed:
#   staging-runner              master + runtime DB passwords (execution-role
#                               injection, env DB_MASTER_PASSWORD / DB_RUNTIME_PASSWORD,
#                               never PGPASSWORD: the scripts build 0600 passfiles and
#                               unset the variables) and the RUNNER TASK ROLE, whose only
#                               permission is rds-db:connect as igaming_backup on this
#                               instance (modules/iam).
#   staging-runner-actor-proof  master password and ACTOR_PROOF_KEYS only, and NO task
#                               role at all (no AWS credentials in the container).
# Both use the one-off execution role (ECR pull of the runner repository, the secrets
# above, the runner log group), never the service execution role.
locals {
  runner_common_environment = [
    { name = "APP_ENV", value = var.app_environment },
    { name = "AWS_REGION", value = var.aws_region },
    { name = "AWS_DEFAULT_REGION", value = var.aws_region },
    { name = "RDS_HOST", value = var.database_host },
    { name = "RDS_PORT", value = tostring(var.database_port) },
    { name = "DB_NAME", value = var.database_name },
    { name = "DB_MASTER_USER", value = var.db_master_username },
    { name = "RDS_CA_BUNDLE_URL", value = var.rds_ca_bundle_url },
    { name = "ACTOR_PROOF_ACTIVE_KID", value = var.actor_proof_active_kid },
  ]
}

resource "aws_ecs_task_definition" "staging_runner" {
  family                   = "${var.name_prefix}-staging-runner"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.runner_task_cpu
  memory                   = var.runner_task_memory
  execution_role_arn       = var.one_off_execution_role_arn
  task_role_arn            = var.runner_task_role_arn

  container_definitions = jsonencode([
    {
      name      = "staging-runner"
      image     = var.staging_runner_image
      essential = true
      command   = ["usage"]
      user      = "10001"
      # SRP3-9: ECS sends SIGTERM and SIGKILLs after stopTimeout; 120 s is the Fargate maximum, so the exit trap (drop the loader role and the
      # restore-check database) has the longest possible chance to run. The cleanup mode removes whatever a hard kill leaves behind.
      stopTimeout = 120
      linuxParameters = {
        initProcessEnabled = true
        capabilities       = { drop = ["ALL"] }
      }
      environment = concat(local.runner_common_environment, [
        { name = "DB_RUNTIME_USER", value = var.db_runtime_username },
      ], var.mock_geolocation_country != "" ? [{ name = "MOCK_GEOLOCATION_COUNTRY", value = var.mock_geolocation_country }] : [])
      secrets = [
        { name = "DB_MASTER_PASSWORD", valueFrom = "${var.db_master_secret_arn}:password::" },
        { name = "DB_RUNTIME_PASSWORD", valueFrom = "${var.db_runtime_secret_arn}:password::" },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["staging-runner"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "staging-runner"
        }
      }
    }
  ])

  tags = var.tags
}

resource "aws_ecs_task_definition" "staging_runner_actor_proof" {
  family                   = "${var.name_prefix}-staging-runner-actor-proof"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = var.one_off_execution_role_arn
  # No task_role_arn: this definition holds the actor-proof key material and must
  # carry no AWS credentials at all (it needs none: the secrets are injected by
  # the execution role at start).

  container_definitions = jsonencode([
    {
      name      = "staging-runner"
      image     = var.staging_runner_image
      essential = true
      command   = ["usage"]
      user      = "10001"
      # SRP3-9: ECS sends SIGTERM and SIGKILLs after stopTimeout; 120 s is the Fargate maximum, so the exit trap (drop the loader role and the
      # restore-check database) has the longest possible chance to run. The cleanup mode removes whatever a hard kill leaves behind.
      stopTimeout = 120
      linuxParameters = {
        initProcessEnabled = true
        capabilities       = { drop = ["ALL"] }
      }
      environment = local.runner_common_environment
      secrets = [
        { name = "DB_MASTER_PASSWORD", valueFrom = "${var.db_master_secret_arn}:password::" },
        { name = "ACTOR_PROOF_KEYS", valueFrom = var.actor_proof_keys_secret_arn },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["staging-runner"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "staging-runner-actor-proof"
        }
      }
    }
  ])

  tags = var.tags
}

# STAGING-PREP-3 round 2 (SA-1): the third definition, for mode seed-world only. It needs the master password
# (the owner connection of cmd/seed-staging-world), the staff MFA key families (the same ones platform-api
# and seed-admin use; the bootstrap MFA enrolment token is sealed with them) and a task role that can
# Get/PutSecretValue on the ONE secret igaming-staging/seed-world-passwords. No runtime password, no
# actor-proof key, no rds-db:connect.
resource "aws_ecs_task_definition" "staging_runner_seed_world" {
  family                   = "${var.name_prefix}-staging-runner-seed-world"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = var.one_off_execution_role_arn
  task_role_arn            = var.runner_seed_world_task_role_arn

  container_definitions = jsonencode([
    {
      name      = "staging-runner"
      image     = var.staging_runner_image
      essential = true
      command   = ["usage"]
      user      = "10001"
      # SRP3-9: ECS sends SIGTERM and SIGKILLs after stopTimeout; 120 s is the Fargate maximum, so the exit trap (drop the loader role and the
      # restore-check database) has the longest possible chance to run. The cleanup mode removes whatever a hard kill leaves behind.
      stopTimeout = 120
      linuxParameters = {
        initProcessEnabled = true
        capabilities       = { drop = ["ALL"] }
      }
      environment = concat(local.runner_common_environment, [
        { name = "STAFF_MFA_FACTOR_ACTIVE_KID", value = var.staff_mfa_active_kid },
        { name = "STAFF_MFA_MAC_ACTIVE_KID", value = var.staff_mfa_active_kid },
        { name = "SEED_WORLD_SECRET_ARN", value = var.seed_world_secret_arn },
      ], var.mock_geolocation_country != "" ? [{ name = "MOCK_GEOLOCATION_COUNTRY", value = var.mock_geolocation_country }] : [])
      secrets = [
        { name = "DB_MASTER_PASSWORD", valueFrom = "${var.db_master_secret_arn}:password::" },
        { name = "STAFF_MFA_FACTOR_KEYS", valueFrom = "${var.staff_mfa_keys_secret_arn}:factor_keys::" },
        { name = "STAFF_MFA_MAC_KEYS", valueFrom = "${var.staff_mfa_keys_secret_arn}:mac_keys::" },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this["staging-runner"].name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "staging-runner-seed-world"
        }
      }
    }
  ])

  tags = var.tags
}
