mock_provider "aws" {}

variables {
  name_prefix        = "igaming-staging"
  private_subnet_ids = ["subnet-0aaaaaaaaaaaaaaa1", "subnet-0aaaaaaaaaaaaaaa2"]
  security_group_id  = "sg-0aaaaaaaaaaaaaaa1"
  engine_version     = "16.15"
}

run "rds_is_private_and_owns_its_password" {
  command = plan

  assert {
    condition     = aws_db_instance.this.publicly_accessible == false
    error_message = "RDS must never be publicly accessible."
  }

  assert {
    condition     = aws_db_instance.this.manage_master_user_password == true && aws_db_instance.this.password == null
    error_message = "RDS must generate/store the master password itself; Terraform must never pass one."
  }

  assert {
    condition     = aws_db_instance.this.storage_encrypted == true
    error_message = "Storage must be encrypted at rest."
  }

  assert {
    condition     = length(aws_kms_key.rds) == 1
    error_message = "Module default must keep the dedicated customer-managed key."
  }
}

run "aws_managed_key_opt_in" {
  command = plan

  variables {
    create_kms_key = false
  }

  assert {
    # kms_key_id itself is provider-computed (unknown at plan): with no
    # customer-managed key, RDS falls back to the AWS-managed aws/rds key.
    condition     = length(aws_kms_key.rds) == 0 && length(aws_kms_alias.rds) == 0 && aws_db_instance.this.storage_encrypted == true
    error_message = "create_kms_key = false must use the AWS-managed key while staying encrypted."
  }
}

run "engine_version_has_no_default" {
  command = plan

  variables {
    engine_version = "latest"
  }

  expect_failures = [var.engine_version]
}

# --- A12 / ADR 0122: managed PITR, final snapshot, audit logging (prepared, not applied) ---

run "pitr_snapshot_and_audit_settings" {
  command = plan

  variables {
    backup_retention_days           = 14
    backup_window                   = "01:00-02:00"
    deletion_protection             = true
    skip_final_snapshot             = false
    final_snapshot_identifier       = "igaming-staging-db-final-abc1234"
    enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]
    enable_pgaudit                  = true
  }

  assert {
    condition     = aws_db_instance.this.backup_retention_period == 14 && aws_db_instance.this.backup_window == "01:00-02:00"
    error_message = "Automated backups (the PITR window) must be 14 days with the configured backup window."
  }

  assert {
    condition     = aws_db_instance.this.deletion_protection == true && aws_db_instance.this.skip_final_snapshot == false && aws_db_instance.this.final_snapshot_identifier == "igaming-staging-db-final-abc1234"
    error_message = "Deletion protection on and a named final snapshot are required."
  }

  assert {
    condition     = aws_db_instance.this.copy_tags_to_snapshot == true && aws_db_instance.this.storage_encrypted == true
    error_message = "Snapshots must carry the tags and stay encrypted."
  }

  assert {
    condition     = toset(aws_db_instance.this.enabled_cloudwatch_logs_exports) == toset(["postgresql", "upgrade"]) && length(aws_cloudwatch_log_group.rds_export) == 2
    error_message = "postgresql and upgrade logs must be exported to pre-created, retention-managed log groups."
  }

  assert {
    condition = alltrue([
      for n in ["rds.force_ssl", "log_connections", "log_disconnections", "shared_preload_libraries"] :
      contains([for p in aws_db_parameter_group.this.parameter : p.name], n)
    ])
    error_message = "Parameter group must force SSL, log connections and disconnections, and preload pgaudit when enabled."
  }
}

run "pgaudit_is_opt_in" {
  command = plan

  assert {
    condition     = !contains([for p in aws_db_parameter_group.this.parameter : p.name], "shared_preload_libraries")
    error_message = "shared_preload_libraries (a reboot-requiring parameter) must only be set when enable_pgaudit = true."
  }
}

run "customer_managed_key_reference_takes_precedence" {
  command = plan

  variables {
    kms_key_arn = "arn:aws:kms:eu-central-1:765578795051:key/11111111-2222-3333-4444-555555555555"
  }

  assert {
    condition     = aws_db_instance.this.kms_key_id == "arn:aws:kms:eu-central-1:765578795051:key/11111111-2222-3333-4444-555555555555"
    error_message = "An explicit kms_key_arn must be the storage/snapshot/PITR key."
  }
}

run "final_snapshot_requires_an_identifier" {
  command = plan

  variables {
    skip_final_snapshot = false
  }

  expect_failures = [aws_db_instance.this]
}

run "zero_retention_would_disable_pitr_and_is_rejected" {
  command = plan

  variables {
    backup_retention_days = 0
  }

  expect_failures = [var.backup_retention_days]
}
