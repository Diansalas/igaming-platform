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

output "seed_admin_password_secret_arn" {
  description = "Plain-string secret consumed by the one-off seed-admin task as SEED_ADMIN_PASSWORD."
  value       = aws_secretsmanager_secret.seed_admin.arn
}

output "secret_version_ids" {
  description = "Secret-version resource IDs. Consumers depend on these so no task starts before a value exists."
  value = [
    aws_secretsmanager_secret_version.db_runtime.id,
    aws_secretsmanager_secret_version.jwt_signing.id,
    aws_secretsmanager_secret_version.seed_admin.id,
  ]
}
