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
  description = "RDS resource ID, used for CloudWatch alarm dimensions."
  value       = aws_db_instance.this.resource_id
}

output "kms_key_arn" {
  value = aws_kms_key.rds.arn
}
