output "address" {
  description = "RDS endpoint hostname (no port, no credentials)."
  value       = aws_db_instance.this.address
}

output "port" {
  value = aws_db_instance.this.port
}

output "db_name" {
  value = aws_db_instance.this.db_name
}

output "identifier" {
  value = aws_db_instance.this.identifier
}

output "resource_id" {
  description = "RDS resource ID (DbiResourceId). NOT a valid dimension for the AWS/RDS CPUUtilization/FreeStorageSpace metrics — alarms must use `identifier` (DBInstanceIdentifier) instead."
  value       = aws_db_instance.this.resource_id
}

output "master_username" {
  value = aws_db_instance.this.username
}

output "master_user_secret_arn" {
  description = "ARN of the RDS-managed Secrets Manager secret holding the master credentials (JSON keys \"username\" and \"password\"). The value itself never passes through Terraform."
  value       = aws_db_instance.this.master_user_secret[0].secret_arn
}

output "kms_key_arn" {
  description = "Null when create_kms_key = false (AWS-managed aws/rds key)."
  value       = one(aws_kms_key.rds[*].arn)
}
