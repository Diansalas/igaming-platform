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
    db_master_rds_managed = module.database.master_user_secret_arn
    db_runtime            = module.secrets.db_runtime_secret_arn
    jwt_signing           = module.secrets.jwt_signing_secret_arn
    seed_admin_password   = module.secrets.seed_admin_password_secret_arn
  }
}

output "sns_alerts_topic_arn" {
  description = "Null unless alarm_email is set."
  value       = module.observability.sns_topic_arn
}
