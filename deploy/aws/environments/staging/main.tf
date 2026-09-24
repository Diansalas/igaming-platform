# Staging environment — root module wiring every deploy/aws/modules/*
# module together. Architecture: ADR 0084 (Stage 9.3), amended by ADR 0086
# (Stage 9.4 hardening + cost optimization). Operator procedure:
# docs/runbooks/stage-9-4-staging-lifecycle-runbook.md.
#
# Shape (ADR 0086):
#
#   browser/acceptance suite (allowlisted IPv4 only)
#     --HTTPS--> 3 CloudFront distributions (*.cloudfront.net, IP allowlist)
#     --VPC origin--> internal ALB (private subnets, header routing)
#     --> ECS Fargate tasks (public subnets + public IP, ingress ALB-only;
#         no NAT Gateway)
#     --> RDS PostgreSQL 16.15 (private subnets, never public)
#
# This is an EPHEMERAL acceptance environment: create -> deploy -> accept
# -> inspect -> destroy. Nothing here is meant to run continuously.

data "aws_caller_identity" "current" {}

locals {
  name_prefix = var.name_prefix

  # One origin custom header, shared by modules/edge (sets it) and
  # modules/alb (routes on it).
  routing_header_name = "X-Igaming-Service"

  # Deterministic CloudWatch log group names/ARNs, computed WITHOUT
  # depending on the ecs module's own resources — this is what lets the
  # iam module (created before ecs) scope its execution-role policies to
  # these exact log groups without a module dependency cycle (ecs also
  # needs iam's role ARNs). The ecs module independently creates log
  # groups with names built the identical way from the same name_prefix.
  service_log_keys = ["platform-api", "b2c", "backoffice"]
  one_off_log_keys = ["migrate", "role-init", "seed-admin"]
  log_group_names  = { for k in concat(local.service_log_keys, local.one_off_log_keys) : k => "/ecs/${local.name_prefix}/${k}" }
  log_group_arn    = { for k, name in local.log_group_names : k => "arn:aws:logs:${var.aws_region}:${data.aws_caller_identity.current.account_id}:log-group:${name}:*" }

  # Frontends are cross-origin from the API: each app has its own
  # CloudFront distribution/origin. CORS_ALLOWED_ORIGINS names both exact
  # browser origins.
  cors_allowed_origins = "${module.edge.urls["b2c"]},${module.edge.urls["backoffice"]}"

  # Staging capacity strategy (ADR 0086). Frontends: Spot only (an
  # interruption briefly takes a static site down; acceptable for
  # staging). platform-api: the first task always on on-demand FARGATE, so
  # acceptance runs are not exposed to Spot interruption; any extra
  # replica (the multi-replica test) on Spot.
  spot_only = [{ capacity_provider = "FARGATE_SPOT", base = 0, weight = 1 }]
  platform_api_strategy = [
    { capacity_provider = "FARGATE", base = 1, weight = 0 },
    { capacity_provider = "FARGATE_SPOT", base = 0, weight = 1 },
  ]
}

module "network" {
  source = "../../modules/network"

  name_prefix        = local.name_prefix
  vpc_cidr           = var.vpc_cidr
  az_count           = var.az_count
  enable_nat_gateway = !var.ecs_public_ip_mode
  tags               = var.tags
}

module "security" {
  source = "../../modules/security"

  name_prefix = local.name_prefix
  vpc_id      = module.network.vpc_id
  # ALB ingress is the CloudFront origin-facing managed prefix list, set
  # inside the module (the only documented VPC-origin source besides the
  # service-managed SG). No subnet — public or private — is admitted.
  tags = var.tags
}

module "ecr" {
  source = "../../modules/ecr"

  name_prefix  = local.name_prefix
  force_delete = true # disposable staging: teardown must not leave images billing
  tags         = var.tags
}

module "secrets" {
  source = "../../modules/secrets"

  name_prefix             = local.name_prefix
  secret_version          = var.secret_version
  recovery_window_in_days = 0 # disposable staging: force delete, names reusable at once
  tags                    = var.tags
}

module "database" {
  source = "../../modules/database"

  name_prefix           = local.name_prefix
  private_subnet_ids    = module.network.private_subnet_ids
  security_group_id     = module.security.rds_security_group_id
  instance_class        = var.db_instance_class
  allocated_storage     = var.db_allocated_storage
  engine_version        = var.db_engine_version
  db_name               = var.db_name
  backup_retention_days = var.db_backup_retention_days
  multi_az              = var.db_multi_az
  deletion_protection   = var.db_deletion_protection
  create_kms_key        = false # AWS-managed aws/rds key; no CMK left pending deletion per teardown
  # engine_version is pinned exactly (16.15) — keep RDS from drifting it.
  auto_minor_version_upgrade = false
  tags                       = var.tags
}

module "iam" {
  source = "../../modules/iam"

  name_prefix = local.name_prefix

  service_ecr_repository_arns = values(module.ecr.repository_arns)
  service_secret_arns = [
    module.secrets.db_runtime_secret_arn,
    module.secrets.jwt_signing_secret_arn,
  ]
  service_log_group_arns = [for k in local.service_log_keys : local.log_group_arn[k]]

  one_off_ecr_repository_arns = [module.ecr.repository_arns["platform-api"]]
  one_off_secret_arns = [
    module.database.master_user_secret_arn,
    module.secrets.db_runtime_secret_arn,
    module.secrets.seed_admin_password_secret_arn,
  ]
  one_off_log_group_arns = [for k in local.one_off_log_keys : local.log_group_arn[k]]

  # Created once by deploy/aws/bootstrap; the deployer policy only permits
  # creating/editing roles that carry exactly this boundary (ADR 0086).
  permissions_boundary_arn = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:policy/${local.name_prefix}-ecs-role-boundary"

  tags = var.tags
}

module "alb" {
  source = "../../modules/alb"

  name_prefix         = local.name_prefix
  vpc_id              = module.network.vpc_id
  internal            = true
  subnet_ids          = module.network.private_subnet_ids
  security_group_id   = module.security.alb_security_group_id
  routing_header_name = local.routing_header_name
  container_port      = 8080
  tags                = var.tags
}

module "edge" {
  source = "../../modules/edge"

  name_prefix          = local.name_prefix
  alb_arn              = module.alb.alb_arn
  alb_dns_name         = module.alb.dns_name
  routing_header_name  = local.routing_header_name
  allowed_viewer_cidrs = var.staging_access_cidrs
  tags                 = var.tags
}

module "ecs" {
  source = "../../modules/ecs"

  name_prefix                = local.name_prefix
  aws_region                 = var.aws_region
  task_subnet_ids            = var.ecs_public_ip_mode ? module.network.public_subnet_ids : module.network.private_subnet_ids
  assign_public_ip           = var.ecs_public_ip_mode
  container_insights_enabled = false
  ecs_security_group_id      = module.security.ecs_security_group_id
  service_execution_role_arn = module.iam.service_execution_role_arn
  one_off_execution_role_arn = module.iam.one_off_execution_role_arn
  task_role_arn              = module.iam.task_role_arn
  log_retention_days         = var.log_retention_days
  container_port             = 8080

  platform_api_image = "${module.ecr.repository_urls["platform-api"]}:${var.image_tag}"
  b2c_image          = "${module.ecr.repository_urls["b2c"]}:${var.image_tag}"
  backoffice_image   = "${module.ecr.repository_urls["backoffice"]}:${var.image_tag}"

  platform_api_desired_count = var.platform_api_desired_count
  b2c_desired_count          = var.b2c_desired_count
  backoffice_desired_count   = var.backoffice_desired_count

  platform_api_capacity_provider_strategy = local.platform_api_strategy
  b2c_capacity_provider_strategy          = local.spot_only
  backoffice_capacity_provider_strategy   = local.spot_only

  app_environment      = "staging"
  cors_allowed_origins = local.cors_allowed_origins
  # viewer -> CloudFront (appends the viewer IP to X-Forwarded-For) -> ALB
  # (appends CloudFront's address) -> task: the real client is 2 hops from
  # the right (internal/httpserver trustedProxyClientIP).
  trusted_proxy_count = 2

  # Stage 9.4: staging is exactly where the three test-support routes
  # (casino play simulation, mock payment settlement, account-activation
  # test support) are needed — the staging acceptance-test flows depend
  # on them. See modules/ecs/variables.tf's own doc comment: this module
  # defaults to false, so this environment deliberately opts in. They are
  # reachable only by allowlisted viewers (modules/edge, ADR 0086).
  test_support_endpoints_enabled = true

  target_group_arns = module.alb.target_group_arns

  database_host        = module.database.address
  database_port        = module.database.port
  database_name        = module.database.db_name
  db_runtime_username  = module.secrets.db_runtime_username
  db_master_username   = module.database.master_username
  db_master_secret_arn = module.database.master_user_secret_arn

  db_runtime_secret_arn          = module.secrets.db_runtime_secret_arn
  jwt_signing_secret_arn         = module.secrets.jwt_signing_secret_arn
  seed_admin_password_secret_arn = module.secrets.seed_admin_password_secret_arn

  tags = var.tags

  # ECS services attach to ALB target groups, so the ALB's listener must
  # exist first; and no task may start before its secret VALUES exist.
  depends_on = [module.alb, module.secrets]
}

module "observability" {
  source = "../../modules/observability"

  name_prefix               = local.name_prefix
  alb_arn_suffix            = module.alb.alb_arn_suffix
  target_group_arn_suffixes = module.alb.target_group_arn_suffixes
  rds_instance_identifier   = module.database.identifier
  alarm_email               = var.alarm_email
  tags                      = var.tags
}
