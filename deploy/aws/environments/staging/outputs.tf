output "aws_region" {
  value = var.aws_region
}

output "image_tag" {
  description = "The immutable image identity (full git SHA) this apply deployed."
  value       = var.image_tag
}

output "staging_url" {
  description = "B2C player frontend (HTTPS, CloudFront default certificate, allowlisted viewers only)."
  value       = module.edge.urls["b2c"]
}

output "backoffice_url" {
  description = "Back Office frontend (HTTPS, allowlisted viewers only)."
  value       = module.edge.urls["backoffice"]
}

output "api_url" {
  description = "platform-api base URL (HTTPS only, allowlisted viewers only). Baked into the frontend images at build time by deploy.sh."
  value       = module.edge.urls["platform_api"]
}

output "cloudfront_distribution_ids" {
  value = module.edge.distribution_ids
}

output "alb_dns_name" {
  description = "INTERNAL ALB DNS name — resolvable/reachable only from inside the VPC. Use the CloudFront URLs above from anywhere else."
  value       = module.alb.dns_name
}

output "rds_endpoint" {
  description = "RDS hostname (no port, no credentials). Private: reachable only from the ECS tasks security group."
  value       = module.database.address
}

output "rds_port" {
  value = module.database.port
}

output "database_recovery" {
  description = "A12 / ADR 0122 recovery posture of the staging database (settings only; PREPARED, NOT APPLIED until the staging deploy). latest_restorable_time is the newest point PITR can restore to right now."
  value = {
    backup_retention_days  = module.database.backup_retention_period
    deletion_protection    = module.database.deletion_protection
    latest_restorable_time = module.database.latest_restorable_time
    backup_runner_secret   = var.backup_runner_secret_arn # reference only; Terraform never holds the value
  }
}

output "ecr_repository_urls" {
  value = module.ecr.repository_urls
}

output "ecs_cluster_name" {
  value = module.ecs.cluster_name
}

output "ecs_service_names" {
  value = module.ecs.service_names
}

output "ecs_migrate_task_definition_arn" {
  value = module.ecs.task_definition_arns["migrate"]
}

output "ecs_role_init_task_definition_arn" {
  value = module.ecs.task_definition_arns["role_init"]
}

output "ecs_seed_admin_task_definition_arn" {
  value = module.ecs.task_definition_arns["seed_admin"]
}

output "ecs_staging_runner_task_definition_arn" {
  description = "STAGING-PREP-3: the one-off staging runner (deploy.sh runner <mode>): verify-db, seed-market, backup-restore-drill, pitr-verify, probe-backup-role."
  value       = module.ecs.task_definition_arns["staging_runner"]
}

output "ecs_staging_runner_actor_proof_task_definition_arn" {
  description = "STAGING-PREP-3: the staging-runner definition that carries the actor-proof key material and no task role (mode seed-actor-proof only)."
  value       = module.ecs.task_definition_arns["staging_runner_actor_proof"]
}

output "ecs_staging_runner_seed_world_task_definition_arn" {
  description = "STAGING-PREP-3 round 2 (SA-1): the staging-runner definition for mode seed-world: master password, staff MFA keys and the seed-world task role (Get/PutSecretValue on igaming-staging/seed-world-passwords only)."
  value       = module.ecs.task_definition_arns["staging_runner_seed_world"]
}

output "runner_seed_world_task_role_arn" {
  description = "SA-1: the seed-world task role."
  value       = module.iam.runner_seed_world_task_role_arn
}

output "runner_task_role_arn" {
  description = "STAGING-PREP-3: the staging-runner task role (rds-db:connect as igaming_backup on this instance only)."
  value       = module.iam.runner_task_role_arn
}

output "db_resource_id" {
  description = "DbiResourceId of the staging instance (not secret; names the rds-db:connect ARN)."
  value       = module.database.resource_id
}

output "key_family_kid" {
  description = "Active kid of every key family (staff MFA, payout instrument, actor proof); the seed-actor-proof mode requires it to equal the kid inside the actor-proof secret."
  value       = module.secrets.key_family_kid
}

output "ecs_task_subnet_ids" {
  description = "Subnets deploy.sh runs the one-off tasks in (same as the services)."
  value       = var.ecs_public_ip_mode ? module.network.public_subnet_ids : module.network.private_subnet_ids
}

output "ecs_assign_public_ip" {
  description = "\"ENABLED\"/\"DISABLED\" for aws ecs run-task's awsvpcConfiguration.assignPublicIp."
  value       = var.ecs_public_ip_mode ? "ENABLED" : "DISABLED"
}

output "ecs_security_group_id" {
  value = module.security.ecs_security_group_id
}

output "log_group_names" {
  value = local.log_group_names
}

output "secret_arns" {
  description = "Secrets Manager ARNs only — never the underlying values (which are not in Terraform state either; ADR 0086)."
  value = {
    db_master_rds_managed  = module.database.master_user_secret_arn
    db_runtime             = module.secrets.db_runtime_secret_arn
    jwt_signing            = module.secrets.jwt_signing_secret_arn
    seed_admin_password    = module.secrets.seed_admin_password_secret_arn
    staff_mfa_keys         = module.secrets.staff_mfa_keys_secret_arn
    payout_instrument_keys = module.secrets.payout_instrument_keys_secret_arn
    actor_proof_keys       = module.secrets.actor_proof_keys_secret_arn
    alert_webhook_hmac     = module.secrets.alert_webhook_hmac_secret_arn
    seed_world_passwords   = module.secrets.seed_world_secret_arn # empty container; written only by the seed-world runner task
  }
}

output "sns_alerts_topic_arn" {
  description = "Null unless alarm_email is set."
  value       = module.observability.sns_topic_arn
}
