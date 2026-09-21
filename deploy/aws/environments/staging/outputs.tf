output "aws_region" {
  value = var.aws_region
}

output "staging_url" {
  description = "The primary way to reach the b2c staging frontend. HTTPS custom domain if domain_name+route53_zone_id were supplied; otherwise the ALB's own DNS name over plain HTTP (see alb_dns_name_fallback_warning)."
  value       = local.domain_configured ? "https://${local.app_hostname}" : "http://${module.alb.dns_name}"
}

output "backoffice_url" {
  value = local.domain_configured ? "https://${local.admin_hostname}" : "http://${module.alb.dns_name}"
}

output "api_url" {
  value = local.domain_configured ? "https://${local.api_hostname}" : "http://${module.alb.dns_name}"
}

output "alb_dns_name" {
  description = "Raw ALB DNS name. Always populated; use with a Host header override to reach a specific service when no domain is configured (see the runbook)."
  value       = module.alb.dns_name
}

output "alb_dns_name_fallback_warning" {
  value = local.domain_configured ? "HTTPS is enabled via the supplied domain; this ALB DNS name still works for direct/debug access with a Host header." : "NON-HTTPS TEMPORARY FALLBACK: no domain_name/route53_zone_id was supplied, so this deployment serves plain HTTP only on the ALB's own *.elb.amazonaws.com name. Host-based routing to b2c/backoffice requires a Host header override (curl -H \"Host: ${local.app_hostname}\" ...) since there is no real DNS entry for that hostname. Supply domain_name + route53_zone_id for a real HTTPS staging URL."
}

output "rds_endpoint" {
  description = "RDS hostname (no port, no credentials)."
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

output "log_group_names" {
  value = local.log_group_names
}

output "secret_arns" {
  description = "Secrets Manager ARNs only — never the underlying values."
  value = {
    db_master              = module.secrets.db_master_secret_arn
    db_runtime             = module.secrets.db_runtime_secret_arn
    jwt_signing            = module.secrets.jwt_signing_secret_arn
    database_url_runtime   = aws_secretsmanager_secret.database_url_runtime.arn
    database_url_migration = aws_secretsmanager_secret.database_url_migration.arn
  }
}

output "sns_alerts_topic_arn" {
  value = module.observability.sns_topic_arn
}

output "private_subnet_ids" {
  value = module.network.private_subnet_ids
}

output "ecs_security_group_id" {
  value = module.security.ecs_security_group_id
}
