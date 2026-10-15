# RDS PostgreSQL 16, private (no public access, no public subnet),
# encrypted at rest, rds.force_ssl enforced, automated backups. Sizing
# (instance class, multi_az, deletion_protection) is entirely
# variable-driven so this exact module can be reused for a future
# `environments/production` root module with different values, per the
# "swap staging sizing for production sizing without rewriting the module"
# requirement.
#
# MASTER PASSWORD (ADR 0086): RDS generates and stores the master password
# itself in an RDS-managed Secrets Manager secret
# (manage_master_user_password = true). Terraform never sees the value, so
# it is never written to Terraform state or plan files — unlike the Stage
# 9.3 design, which generated it with random_password and passed it in.
# Consumers receive only the secret's ARN (output master_user_secret_arn)
# and inject its "password" JSON key into containers at task start.
#
# ENCRYPTION KEY: create_kms_key (default true) creates a dedicated
# customer-managed KMS key, as Stage 9.3 did. The staging root sets it
# false and uses the AWS-managed aws/rds key instead (still encrypted at
# rest): a disposable staging environment would otherwise leave a
# pending-deletion customer-managed key behind on every teardown
# (deletion_window_in_days is 7 minimum). See ADR 0086.

resource "aws_kms_key" "rds" {
  count = var.create_kms_key ? 1 : 0

  description             = "${var.name_prefix} RDS encryption-at-rest key"
  deletion_window_in_days = var.kms_deletion_window_days
  enable_key_rotation     = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-rds-kms" })
}

resource "aws_kms_alias" "rds" {
  count = var.create_kms_key ? 1 : 0

  name          = "alias/${var.name_prefix}-rds"
  target_key_id = aws_kms_key.rds[0].key_id
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

  # A12 / ADR 0122 audit trail for the dedicated backup role (and everyone else): who connected, from where, and
  # when the session ended. Statement-level auditing is pgaudit (below); plain log_statement is not used.
  parameter {
    name         = "log_connections"
    value        = "1"
    apply_method = "immediate"
  }

  parameter {
    name         = "log_disconnections"
    value        = "1"
    apply_method = "immediate"
  }

  # shared_preload_libraries is static: pending-reboot. Only added when enable_pgaudit = true.
  dynamic "parameter" {
    for_each = var.enable_pgaudit ? [1] : []
    content {
      name         = "shared_preload_libraries"
      value        = "pgaudit"
      apply_method = "pending-reboot"
    }
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-pg16" })
}

# Pre-created log groups for the exported RDS logs so that retention is managed (RDS would otherwise create them with
# no expiry). Names are the ones RDS writes to: /aws/rds/instance/<identifier>/<log type>.
resource "aws_cloudwatch_log_group" "rds_export" {
  for_each = toset(var.enabled_cloudwatch_logs_exports)

  name              = "/aws/rds/instance/${var.name_prefix}-db/${each.value}"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, { Name = "${var.name_prefix}-rds-${each.value}-logs" })
}

resource "aws_db_instance" "this" {
  identifier     = "${var.name_prefix}-db"
  engine         = "postgres"
  engine_version = var.engine_version

  instance_class    = var.instance_class
  allocated_storage = var.allocated_storage
  storage_type      = "gp3"
  storage_encrypted = true
  # precedence: an existing CMK passed by ARN (A12) > the module-created CMK > null (the AWS-managed aws/rds key).
  # The key also protects automated backups, snapshots and the PITR transaction logs.
  kms_key_id = var.kms_key_arn != null ? var.kms_key_arn : (var.create_kms_key ? aws_kms_key.rds[0].arn : null)

  db_name  = var.db_name
  username = var.master_username
  port     = 5432

  # RDS owns the master password (see file header): never in Terraform
  # state. The RDS-managed secret uses the default aws/secretsmanager key.
  manage_master_user_password = true

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [var.security_group_id]
  parameter_group_name   = aws_db_parameter_group.this.name
  publicly_accessible    = false

  # A12 security review M8 / ADR 0122 section 3: IAM database authentication is the REQUIRED credential mechanism for the backup role on
  # RDS (GRANT rds_iam TO igaming_backup, no password). Enabling it changes no existing login: password users keep working.
  iam_database_authentication_enabled = true

  multi_az                = var.multi_az
  backup_retention_period = var.backup_retention_days
  backup_window           = var.backup_window
  maintenance_window      = var.maintenance_window
  deletion_protection     = var.deletion_protection

  # A12 / ADR 0122: the module default (true) keeps the legacy disposable behaviour; the staging root sets false
  # and names the final snapshot. Production must set false.
  skip_final_snapshot       = var.skip_final_snapshot
  final_snapshot_identifier = var.skip_final_snapshot ? null : var.final_snapshot_identifier

  enabled_cloudwatch_logs_exports = var.enabled_cloudwatch_logs_exports

  depends_on = [aws_cloudwatch_log_group.rds_export]

  # With an exact engine_version pin, an AWS-applied minor upgrade would make
  # the next apply attempt a (failing) downgrade; pinned roots set false.
  auto_minor_version_upgrade = var.auto_minor_version_upgrade
  apply_immediately          = true
  copy_tags_to_snapshot      = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-db" })

  lifecycle {
    precondition {
      condition     = var.skip_final_snapshot || var.final_snapshot_identifier != null
      error_message = "skip_final_snapshot = false requires final_snapshot_identifier (unique per destroy, e.g. igaming-staging-db-final-<short-sha>)."
    }
    precondition {
      condition     = var.backup_retention_days >= 1
      error_message = "backup_retention_days must be >= 1: 0 disables automated backups and point-in-time recovery."
    }
  }
}
