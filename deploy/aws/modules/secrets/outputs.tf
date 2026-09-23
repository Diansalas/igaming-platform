output "db_runtime_username" {
  value = var.db_runtime_username
}

output "db_runtime_secret_arn" {
  description = "JSON {username,password}. Consumers inject the \"password\" key only (\"<arn>:password::\")."
  value       = aws_secretsmanager_secret.db_runtime.arn
}

output "jwt_signing_secret_arn" {
  value = aws_secretsmanager_secret.jwt_signing.arn
}
