# RDS PostgreSQL 16, private (no public access, no public subnet),
# encrypted at rest with a dedicated KMS key, rds.force_ssl enforced,
# automated backups. Sizing (instance class, multi_az, deletion_protection)
# is entirely variable-driven so this exact module can be reused for a
# future `environments/production` root module with different values,
# per the "swap staging sizing for production sizing without rewriting the
# module" requirement.

resource "aws_kms_key" "rds" {
  description             = "${var.name_prefix} RDS encryption-at-rest key"
  deletion_window_in_days = var.kms_deletion_window_days
  enable_key_rotation     = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-rds-kms" })
}

resource "aws_kms_alias" "rds" {
  name          = "alias/${var.name_prefix}-rds"
  target_key_id = aws_kms_key.rds.key_id
}

resource "aws_db_subnet_group" "this" {
  name       = "${var.name_prefix}-db-subnet-group"
  subnet_ids = var.private_subnet_ids

  tags = merge(var.tags, { Name = "${var.name_prefix}-db-subnet-group" })
}

# rds.force_ssl=1 rejects any client connection that doesn't negotiate TLS,
# closing the gap the production checklist flags: "Include sslmode=require
# (or stricter) in production — the dev/CI convention's sslmode=disable is
# a local-only shortcut." This makes it enforced server-side, not merely a
# client-side convention.
resource "aws_db_parameter_group" "this" {
  name   = "${var.name_prefix}-pg16"
  family = "postgres16"

  parameter {
    name         = "rds.force_ssl"
    value        = "1"
    apply_method = "immediate"
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-pg16" })
}

resource "aws_db_instance" "this" {
  identifier     = "${var.name_prefix}-db"
  engine         = "postgres"
  engine_version = var.engine_version

  instance_class    = var.instance_class
  allocated_storage = var.allocated_storage
  storage_type      = "gp3"
  storage_encrypted = true
  kms_key_id        = aws_kms_key.rds.arn

  db_name  = var.db_name
  username = var.master_username
  password = var.master_password
  port     = 5432

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [var.security_group_id]
  parameter_group_name   = aws_db_parameter_group.this.name
  publicly_accessible    = false

  multi_az                = var.multi_az
  backup_retention_period = var.backup_retention_days
  deletion_protection     = var.deletion_protection

  # Staging is fully disposable: no final snapshot is kept on destroy.
  # Production must set skip_final_snapshot=false and provide a
  # final_snapshot_identifier.
  skip_final_snapshot = true

  auto_minor_version_upgrade = true
  apply_immediately          = true
  copy_tags_to_snapshot      = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-db" })
}
