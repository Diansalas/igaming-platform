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

  seed_admin_password_secret_arn = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/seed-admin-password-AbCdEf"

  staff_mfa_keys_secret_arn         = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/staff-mfa-keys-AbCdEf"
  staff_mfa_active_kid              = "stg-v1"
  payout_instrument_keys_secret_arn = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/payout-instrument-keys-AbCdEf"
  payout_instrument_active_kid      = "stg-v1"
  actor_proof_keys_secret_arn       = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/actor-proof-keys-AbCdEf"
  actor_proof_active_kid            = "stg-v1"

  staging_runner_image = "765578795051.dkr.ecr.eu-central-1.amazonaws.com/igaming-staging/staging-runner:0123456789abcdef0123456789abcdef01234567"
  runner_task_role_arn = "arn:aws:iam::765578795051:role/igaming-staging-ecs-runner-task-role"

  runner_seed_world_task_role_arn = "arn:aws:iam::765578795051:role/igaming-staging-ecs-seed-world-task-role"
  seed_world_secret_arn           = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/seed-world-passwords-AbCdEf"
}

run "database_url_never_carries_a_password" {
  command = plan

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.platform_api, aws_ecs_task_definition.migrate, aws_ecs_task_definition.role_init, aws_ecs_task_definition.seed_admin] :
      alltrue([
        for e in lookup(jsondecode(td.container_definitions)[0], "environment", []) :
        can(regex("^postgres://[A-Za-z0-9_]+@[^/:@]+:[0-9]+/[A-Za-z0-9_]+\\?sslmode=require$", e.value)) if e.name == "DATABASE_URL"
      ])
    ])
    error_message = "DATABASE_URL must be postgres://<user>@host:port/db?sslmode=require — never a password component."
  }

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.platform_api, aws_ecs_task_definition.migrate, aws_ecs_task_definition.role_init, aws_ecs_task_definition.seed_admin] :
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
      PGPASSWORD                = "${var.db_runtime_secret_arn}:password::"
      JWT_SIGNING_SECRET        = var.jwt_signing_secret_arn
      STAFF_MFA_FACTOR_KEYS     = "${var.staff_mfa_keys_secret_arn}:factor_keys::"
      STAFF_MFA_MAC_KEYS        = "${var.staff_mfa_keys_secret_arn}:mac_keys::"
      PAYOUT_INSTRUMENT_KEYS    = "${var.payout_instrument_keys_secret_arn}:master_keys::"
      PAYOUT_INSTRUMENT_FP_KEYS = "${var.payout_instrument_keys_secret_arn}:fp_keys::"
      ACTOR_PROOF_KEYS          = var.actor_proof_keys_secret_arn
    }
    error_message = "platform-api must receive only the RUNTIME password (PGPASSWORD), the JWT secret and the staff MFA, payout instrument and actor proof key families (by Secrets Manager reference)."
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
    condition = {
      for s in jsondecode(aws_ecs_task_definition.seed_admin.container_definitions)[0].secrets : s.name => s.valueFrom
      } == {
      PGPASSWORD            = "${var.db_master_secret_arn}:password::"
      SEED_ADMIN_PASSWORD   = var.seed_admin_password_secret_arn
      STAFF_MFA_FACTOR_KEYS = "${var.staff_mfa_keys_secret_arn}:factor_keys::"
      STAFF_MFA_MAC_KEYS    = "${var.staff_mfa_keys_secret_arn}:mac_keys::"
    }
    error_message = "seed-admin must receive only the master password, its own admin password secret and the staff MFA keys (staffmfa.LoadKeys)."
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
    condition     = alltrue([for td in [aws_ecs_task_definition.migrate, aws_ecs_task_definition.role_init, aws_ecs_task_definition.seed_admin] : td.execution_role_arn == var.one_off_execution_role_arn])
    error_message = "migrate/role-init/seed-admin must use the one-off execution role."
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

# --- STAGING-PREP-1 (plan G3/G4/G5, findings F2/F3) ---

run "startup_required_names_present_and_secret_values_absent_from_environment" {
  command = plan

  assert {
    condition = toset([for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment : e.name]) == toset([
      "APP_ENV", "DATABASE_URL", "HTTP_ADDR", "TRUSTED_PROXY_COUNT", "CORS_ALLOWED_ORIGINS", "OTEL_SERVICE_NAME", "OTEL_EXPORTER",
      "TEST_SUPPORT_ENDPOINTS_ENABLED", "STAFF_MFA_FACTOR_ACTIVE_KID", "STAFF_MFA_MAC_ACTIVE_KID", "PAYOUT_INSTRUMENT_ACTIVE_KID",
      "PAYOUT_INSTRUMENT_FP_ACTIVE_KID", "ACTOR_PROOF_ACTIVE_KID",
    ])
    error_message = "With every optional owner input unset the platform-api environment must be exactly the base set (no MOCK_GEOLOCATION_COUNTRY, METRICS_ADDR, ALERT_* or SECRETSTORE_* variable)."
  }

  assert {
    condition = alltrue([
      for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment :
      !can(regex("(KEYS|SECRET|PASSWORD)$", e.name)) || e.name == "ALERT_WEBHOOK_SECRET_REF"
    ])
    error_message = "No *_KEYS, *_SECRET or *_PASSWORD variable may be a plain environment entry: key material comes from Secrets Manager only."
  }

  assert {
    condition = alltrue([
      for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment :
      e.value == "stg-v1" if endswith(e.name, "_ACTIVE_KID") && e.name != "JWT_ACTIVE_KID"
    ])
    error_message = "Active kids must follow the kid variables."
  }
}

run "optional_owner_inputs_become_environment" {
  command = plan

  variables {
    mock_geolocation_country = "MT"
    metrics_addr             = "127.0.0.1:9464"
    alert_retention_days     = 30
    alert_webhook_url        = "https://alerts.example.com/hook"
    alert_webhook_secret_ref = "awssm://arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/alert-webhook-hmac-AbCdEf?versionId=0123456789abcdef0123456789abcdef"
  }

  assert {
    condition = {
      for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment : e.name => e.value
      if contains(["MOCK_GEOLOCATION_COUNTRY", "METRICS_ADDR", "ALERT_RETENTION_DAYS", "ALERT_WEBHOOK_URL", "SECRETSTORE_BACKENDS", "AWS_SECRETSMANAGER_REGION"], e.name)
      } == {
      MOCK_GEOLOCATION_COUNTRY  = "MT"
      METRICS_ADDR              = "127.0.0.1:9464"
      ALERT_RETENTION_DAYS      = "30"
      ALERT_WEBHOOK_URL         = "https://alerts.example.com/hook"
      SECRETSTORE_BACKENDS      = "awssm"
      AWS_SECRETSMANAGER_REGION = "eu-central-1"
    }
    error_message = "Owner inputs must reach platform-api under the exact variable names the binary reads."
  }

  assert {
    condition = anytrue([
      for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment :
      e.name == "ALERT_WEBHOOK_SECRET_REF" && startswith(e.value, "awssm://arn:aws:secretsmanager:")
    ])
    error_message = "The webhook secret is passed as an awssm:// reference, never a value."
  }
}

run "webhook_url_requires_secret_ref" {
  command = plan

  variables {
    alert_webhook_url = "https://alerts.example.com/hook"
  }

  expect_failures = [aws_ecs_task_definition.platform_api]
}

run "metrics_addr_must_be_loopback" {
  command = plan

  variables {
    metrics_addr = "0.0.0.0:9464"
  }

  expect_failures = [var.metrics_addr]
}

run "metrics_addr_must_not_share_the_http_port" {
  command = plan

  variables {
    metrics_addr = "127.0.0.1:8080"
  }

  expect_failures = [aws_ecs_task_definition.platform_api]
}

run "mock_geolocation_country_must_be_iso_alpha2" {
  command = plan

  variables {
    mock_geolocation_country = "malta"
  }

  expect_failures = [var.mock_geolocation_country]
}

run "migrate_revokes_right_after_up_then_verifies" {
  command = plan

  assert {
    condition     = strcontains(jsondecode(aws_ecs_task_definition.migrate.container_definitions)[0].command[2], "/app/migrate up && psql \"$DATABASE_URL\" -v ON_ERROR_STOP=1 -c \"REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime;\" && /app/migrate verify && /app/migrate status")
    error_message = "The migrate task must run the schema_migrations revoke immediately after up (so a failing verify cannot leave the runtime role able to write it), then verify and status, all joined with && so any failure stops the task."
  }
}

run "role_init_is_single_transaction" {
  command = plan

  assert {
    condition     = strcontains(jsondecode(aws_ecs_task_definition.role_init.container_definitions)[0].command[2], "--single-transaction -v ON_ERROR_STOP=1 -f /app/sql/init-runtime-role.rds.sql")
    error_message = "role-init must run psql --single-transaction so a failure cannot leave the runtime role half-provisioned."
  }
}

run "seed_admin_environment_matches_the_mfa_rules" {
  command = plan

  assert {
    condition = {
      for e in jsondecode(aws_ecs_task_definition.seed_admin.container_definitions)[0].environment : e.name => e.value
      } == {
      DATABASE_URL                = "postgres://igaming@igaming-staging-db.abc123.eu-central-1.rds.amazonaws.com:5432/igaming_platform_staging?sslmode=require"
      APP_ENV                     = "staging"
      STAFF_MFA_FACTOR_ACTIVE_KID = "stg-v1"
      STAFF_MFA_MAC_ACTIVE_KID    = "stg-v1"
    }
    error_message = "seed-admin needs APP_ENV and both staff MFA active kids besides DATABASE_URL."
  }
}

# --- STAGING-PREP-3: the one-off staging runner (deploy.sh runner <mode>) ---

run "staging_runner_definitions_inject_only_what_each_mode_needs" {
  command = plan

  assert {
    condition = {
      for s in jsondecode(aws_ecs_task_definition.staging_runner.container_definitions)[0].secrets : s.name => s.valueFrom
      } == {
      DB_MASTER_PASSWORD  = "${var.db_master_secret_arn}:password::"
      DB_RUNTIME_PASSWORD = "${var.db_runtime_secret_arn}:password::"
    }
    error_message = "staging-runner must receive only the master and runtime DB passwords (as DB_*_PASSWORD, never PGPASSWORD: the scripts build 0600 passfiles)."
  }

  assert {
    condition = {
      for s in jsondecode(aws_ecs_task_definition.staging_runner_actor_proof.container_definitions)[0].secrets : s.name => s.valueFrom
      } == {
      DB_MASTER_PASSWORD = "${var.db_master_secret_arn}:password::"
      ACTOR_PROOF_KEYS   = var.actor_proof_keys_secret_arn
    }
    error_message = "staging-runner-actor-proof must receive only the master password and the actor-proof keys."
  }

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.staging_runner, aws_ecs_task_definition.staging_runner_actor_proof] :
      !contains([for s in jsondecode(td.container_definitions)[0].secrets : s.name], "PGPASSWORD") &&
      !contains([for s in jsondecode(td.container_definitions)[0].secrets : s.name], "JWT_SIGNING_SECRET") &&
      !contains([for e in jsondecode(td.container_definitions)[0].environment : e.name], "DATABASE_URL")
    ])
    error_message = "The runner receives no PGPASSWORD, no JWT secret and no DATABASE_URL (it builds its own connection with passfiles)."
  }
}

run "staging_runner_definitions_allow_the_exit_trap_the_longest_stop_timeout" {
  command = plan

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.staging_runner, aws_ecs_task_definition.staging_runner_actor_proof, aws_ecs_task_definition.staging_runner_seed_world] :
      jsondecode(td.container_definitions)[0].stopTimeout == 120
    ])
    error_message = "SRP3-9: every runner definition sets stopTimeout = 120 (the Fargate maximum) so the exit trap can drop the loader role and the restore-check database."
  }
}

run "staging_runner_seed_world_definition_has_only_its_own_inputs" {
  command = plan

  assert {
    condition = {
      for s in jsondecode(aws_ecs_task_definition.staging_runner_seed_world.container_definitions)[0].secrets : s.name => s.valueFrom
      } == {
      DB_MASTER_PASSWORD    = "${var.db_master_secret_arn}:password::"
      STAFF_MFA_FACTOR_KEYS = "${var.staff_mfa_keys_secret_arn}:factor_keys::"
      STAFF_MFA_MAC_KEYS    = "${var.staff_mfa_keys_secret_arn}:mac_keys::"
    }
    error_message = "SA-1: staging-runner-seed-world receives only the master password and the staff MFA key families (no runtime password, no actor-proof key, no JWT secret)."
  }

  assert {
    condition     = aws_ecs_task_definition.staging_runner_seed_world.task_role_arn == var.runner_seed_world_task_role_arn && var.runner_seed_world_task_role_arn != var.runner_task_role_arn && var.runner_seed_world_task_role_arn != var.task_role_arn
    error_message = "SA-1: seed-world assumes its own task role, never the backup-login runner role or the shared task role."
  }

  assert {
    condition = (
      aws_ecs_task_definition.staging_runner_seed_world.execution_role_arn == var.one_off_execution_role_arn &&
      jsondecode(aws_ecs_task_definition.staging_runner_seed_world.container_definitions)[0].user == "10001" &&
      jsondecode(aws_ecs_task_definition.staging_runner_seed_world.container_definitions)[0].command == ["usage"] &&
      jsondecode(aws_ecs_task_definition.staging_runner_seed_world.container_definitions)[0].logConfiguration.options["awslogs-stream-prefix"] == "staging-runner-seed-world" &&
      jsondecode(aws_ecs_task_definition.staging_runner_seed_world.container_definitions)[0].linuxParameters.capabilities.drop == ["ALL"]
    )
    error_message = "SA-1: the seed-world definition uses the one-off execution role, runs as a non-root user with all capabilities dropped, defaults to usage and logs to the runner group with its own stream prefix."
  }

  assert {
    condition     = anytrue([for e in jsondecode(aws_ecs_task_definition.staging_runner_seed_world.container_definitions)[0].environment : e.name == "SEED_WORLD_SECRET_ARN" && e.value == var.seed_world_secret_arn])
    error_message = "SRP3R-6: the seed-world definition addresses the secret by ARN (SEED_WORLD_SECRET_ARN)."
  }

  assert {
    condition = !anytrue([
      for e in jsondecode(aws_ecs_task_definition.staging_runner_seed_world.container_definitions)[0].environment :
      can(regex("(?i)pass|secret|token|private|credential|keys$", e.name)) && !can(regex("ACTIVE_KID$", e.name)) && e.name != "SEED_WORLD_SECRET_ARN" # an ARN (a name), not a value
    ])
    error_message = "No environment variable of the seed-world definition may look like a credential."
  }
}

run "staging_runner_identity_split" {
  command = plan

  assert {
    condition     = aws_ecs_task_definition.staging_runner.task_role_arn == var.runner_task_role_arn && var.runner_task_role_arn != var.task_role_arn
    error_message = "staging-runner assumes its own task role (rds-db:connect), never the role platform-api shares."
  }

  assert {
    condition     = aws_ecs_task_definition.staging_runner_actor_proof.task_role_arn == null || aws_ecs_task_definition.staging_runner_actor_proof.task_role_arn == ""
    error_message = "The actor-proof definition holds key material and must carry NO task role (no AWS credentials in that container)."
  }

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.staging_runner, aws_ecs_task_definition.staging_runner_actor_proof] :
      td.execution_role_arn == var.one_off_execution_role_arn
    ])
    error_message = "Both runner definitions use the one-off execution role, never the service execution role."
  }
}

run "staging_runner_container_hardening_and_logging" {
  command = plan

  assert {
    condition = alltrue([
      for td in [aws_ecs_task_definition.staging_runner, aws_ecs_task_definition.staging_runner_actor_proof] :
      jsondecode(td.container_definitions)[0].user == "10001" &&
      jsondecode(td.container_definitions)[0].command == ["usage"] &&
      jsondecode(td.container_definitions)[0].linuxParameters.capabilities.drop == ["ALL"] &&
      jsondecode(td.container_definitions)[0].logConfiguration.options["awslogs-group"] == "/ecs/igaming-staging/staging-runner"
    ])
    error_message = "The runner runs as a non-root user with all capabilities dropped, defaults to the harmless usage command and logs to /ecs/igaming-staging/staging-runner."
  }

  assert {
    condition     = aws_cloudwatch_log_group.this["staging-runner"].retention_in_days == var.log_retention_days
    error_message = "The runner log group must have the same retention as the other task log groups."
  }

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.staging_runner.container_definitions)[0].logConfiguration.options["awslogs-stream-prefix"] == "staging-runner" &&
      jsondecode(aws_ecs_task_definition.staging_runner_actor_proof.container_definitions)[0].logConfiguration.options["awslogs-stream-prefix"] == "staging-runner-actor-proof"
    )
    error_message = "deploy.sh finds a run's log stream by these two stream prefixes."
  }
}

run "staging_runner_environment_is_non_secret_and_staging_only" {
  command = plan

  assert {
    condition = alltrue([
      for e in jsondecode(aws_ecs_task_definition.staging_runner.container_definitions)[0].environment :
      !can(regex("(?i)(password|secret|token|key_?material)", e.name)) || e.name == "ACTOR_PROOF_ACTIVE_KID"
    ])
    error_message = "No environment variable of the runner may look like a credential (credentials are execution-role secrets)."
  }

  assert {
    condition = alltrue([
      for e in jsondecode(aws_ecs_task_definition.staging_runner.container_definitions)[0].environment :
      e.name != "APP_ENV" || e.value == "staging"
    ]) && anytrue([for e in jsondecode(aws_ecs_task_definition.staging_runner.container_definitions)[0].environment : e.name == "APP_ENV"])
    error_message = "The runner is pinned to APP_ENV=staging."
  }

  assert {
    condition     = !anytrue([for e in jsondecode(aws_ecs_task_definition.staging_runner.container_definitions)[0].environment : e.name == "MOCK_GEOLOCATION_COUNTRY"])
    error_message = "MOCK_GEOLOCATION_COUNTRY reaches the runner only when the owner supplies it (seed-market cross-checks it)."
  }
}

run "staging_runner_gets_the_owner_country_for_the_seed_market_cross_check" {
  command = plan

  variables {
    mock_geolocation_country = "SV"
  }

  assert {
    condition     = anytrue([for e in jsondecode(aws_ecs_task_definition.staging_runner.container_definitions)[0].environment : e.name == "MOCK_GEOLOCATION_COUNTRY" && e.value == "SV"])
    error_message = "seed-market must see the same country the platform-api gate uses."
  }
}

run "staging_runner_ca_bundle_url_is_pinned_to_the_rds_truststore" {
  command = plan

  variables {
    rds_ca_bundle_url = "https://example.com/bundle.pem"
  }

  expect_failures = [var.rds_ca_bundle_url]
}
