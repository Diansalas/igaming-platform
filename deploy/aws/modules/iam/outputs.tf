output "service_execution_role_arn" {
  value = aws_iam_role.execution["service"].arn
}

output "one_off_execution_role_arn" {
  value = aws_iam_role.execution["one_off"].arn
}

output "task_role_arn" {
  value = aws_iam_role.task.arn
}

output "runner_task_role_arn" {
  value = aws_iam_role.runner_task.arn
}

output "runner_seed_world_task_role_arn" {
  value = aws_iam_role.runner_seed_world.arn
}
