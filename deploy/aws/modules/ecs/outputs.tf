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

output "service_desired_counts" {
  value = {
    "platform-api" = var.platform_api_desired_count
    "b2c"          = var.b2c_desired_count
    "backoffice"   = var.backoffice_desired_count
  }
}

output "task_definition_arns" {
  value = {
    platform_api = aws_ecs_task_definition.platform_api.arn
    b2c          = aws_ecs_task_definition.b2c.arn
    backoffice   = aws_ecs_task_definition.backoffice.arn
    migrate      = aws_ecs_task_definition.migrate.arn
    role_init    = aws_ecs_task_definition.role_init.arn
  }
}

output "log_group_names" {
  value = { for k, v in aws_cloudwatch_log_group.this : k => v.name }
}
