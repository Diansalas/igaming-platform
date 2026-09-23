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
