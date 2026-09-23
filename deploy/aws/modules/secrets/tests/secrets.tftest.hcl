# Pins ADR 0086 secret handling: write-only values (never in state) and the
# recovery-window policy (production-safe default, force delete only on
# explicit opt-in). The real hashicorp/random provider is used — it is
# local-only, and mock providers cannot mock ephemeral resources.

mock_provider "aws" {
  mock_resource "aws_secretsmanager_secret" {
    defaults = {
      arn = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/mock-AbCdEf"
    }
  }
}

variables {
  name_prefix = "igaming-staging"
}

run "production_safe_default_recovery_window" {
  command = plan

  assert {
    condition     = aws_secretsmanager_secret.db_runtime.recovery_window_in_days == 30 && aws_secretsmanager_secret.jwt_signing.recovery_window_in_days == 30
    error_message = "Module default must keep the 30-day recovery window."
  }
}

run "disposable_force_delete_opt_in" {
  command = plan

  variables {
    recovery_window_in_days = 0
  }

  assert {
    condition     = aws_secretsmanager_secret.db_runtime.recovery_window_in_days == 0 && aws_secretsmanager_secret.jwt_signing.recovery_window_in_days == 0
    error_message = "recovery_window_in_days = 0 must force-delete both secrets."
  }
}

run "invalid_recovery_window_rejected" {
  command = plan

  variables {
    recovery_window_in_days = 3
  }

  expect_failures = [var.recovery_window_in_days]
}

run "values_are_write_only" {
  command = apply

  assert {
    condition     = aws_secretsmanager_secret_version.db_runtime.secret_string == null && aws_secretsmanager_secret_version.jwt_signing.secret_string == null
    error_message = "Secret values must be written write-only (secret_string_wo), never via secret_string (which is stored in state)."
  }

  assert {
    condition     = aws_secretsmanager_secret_version.db_runtime.secret_string_wo_version == 1 && aws_secretsmanager_secret_version.jwt_signing.secret_string_wo_version == 1
    error_message = "secret_string_wo_version must follow var.secret_version."
  }
}
