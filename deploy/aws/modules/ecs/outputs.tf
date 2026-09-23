output "cluster_name" {
  value = aws_ecs_cluster.this.name
}

output "cluster_arn" {
  value = aws_ecs_cluster.this.arn
}

output "service_names" {
  value = {
    platform_api = aws_ecs_service.platform_api.name
    b2c          = aws_ecs_service.b2c.name
    backoffice   = aws_ecs_service.backoffice.name
  }
}

output "task_definition_arns" {
  value = {
    platform_api = aws_ecs_task_definition.platform_api.arn
    b2c          = aws_ecs_task_definition.b2c.arn
    backoffice   = aws_ecs_task_definition.backoffice.arn
    migrate      = aws_ecs_task_definition.migrate.arn
    role_init    = aws_ecs_task_definition.role_init.arn
    seed_admin   = aws_ecs_task_definition.seed_admin.arn
  }
}

output "log_group_names" {
  value = { for k, v in aws_cloudwatch_log_group.this : k => v.name }
}

output "service_capacity_provider_strategies" {
  description = "map(service => list of {capacity_provider, base, weight}) — for tests and operators."
  value = {
    for k, svc in {
      platform_api = aws_ecs_service.platform_api
      b2c          = aws_ecs_service.b2c
      backoffice   = aws_ecs_service.backoffice
    } : k => [for c in svc.capacity_provider_strategy : { capacity_provider = c.capacity_provider, base = c.base, weight = c.weight }]
  }
}

output "platform_api_environment" {
  description = "platform-api's NON-secret container environment (name => value). Secrets are injected separately and never appear here."
  value       = { for e in jsondecode(aws_ecs_task_definition.platform_api.container_definitions)[0].environment : e.name => e.value }
}
