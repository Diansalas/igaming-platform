output "db_master_username" {
  value = var.db_master_username
}

output "db_master_password" {
  value     = random_password.db_master.result
  sensitive = true
}

output "db_master_secret_arn" {
  value = aws_secretsmanager_secret.db_master.arn
}

output "db_runtime_username" {
  value = var.db_runtime_username
}

output "db_runtime_password" {
  value     = random_password.db_runtime.result
  sensitive = true
}

output "db_runtime_secret_arn" {
  value = aws_secretsmanager_secret.db_runtime.arn
}

output "jwt_signing_secret_arn" {
  value = aws_secretsmanager_secret.jwt_signing.arn
}
