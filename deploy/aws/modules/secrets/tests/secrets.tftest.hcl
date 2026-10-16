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
    condition     = alltrue([for x in [aws_secretsmanager_secret.db_runtime, aws_secretsmanager_secret.jwt_signing, aws_secretsmanager_secret.seed_admin] : x.recovery_window_in_days == 0])
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
    condition     = alltrue([for v in [aws_secretsmanager_secret_version.db_runtime, aws_secretsmanager_secret_version.jwt_signing, aws_secretsmanager_secret_version.seed_admin] : v.secret_string == null])
    error_message = "Secret values must be written write-only (secret_string_wo), never via secret_string (which is stored in state)."
  }

  assert {
    condition     = aws_secretsmanager_secret_version.db_runtime.secret_string_wo_version == 1 && aws_secretsmanager_secret_version.jwt_signing.secret_string_wo_version == 1
    error_message = "secret_string_wo_version must follow var.secret_version."
  }
}

run "app_secret_rotation_does_not_touch_seed_admin" {
  command = apply

  variables {
    secret_version = 2
  }

  assert {
    condition     = aws_secretsmanager_secret_version.db_runtime.secret_string_wo_version == 2 && aws_secretsmanager_secret_version.jwt_signing.secret_string_wo_version == 2
    error_message = "secret_version must rotate the runtime and JWT secrets."
  }

  assert {
    condition     = aws_secretsmanager_secret_version.seed_admin.secret_string_wo_version == 1
    error_message = "Rotating app secrets must not rewrite the seed-admin password (its DB hash would not follow)."
  }
}

# --- STAGING-PREP-1: key-family secrets ---

run "key_family_values_are_write_only_and_versioned_separately" {
  command = apply

  variables {
    secret_version     = 2
    key_family_version = 3
  }

  assert {
    condition = alltrue([
      for v in [
        aws_secretsmanager_secret_version.staff_mfa_keys,
        aws_secretsmanager_secret_version.payout_instrument_keys,
        aws_secretsmanager_secret_version.actor_proof_keys,
        aws_secretsmanager_secret_version.alert_webhook_hmac,
      ] : v.secret_string == null && v.secret_string_wo_version == 3
    ])
    error_message = "Key-family secrets must be write-only and follow key_family_version, not secret_version."
  }

  assert {
    condition     = output.key_family_kid == "stg-v3"
    error_message = "The kid must carry the key_family_version."
  }

  assert {
    condition     = startswith(output.alert_webhook_secret_ref, "awssm://arn:aws:secretsmanager:") && strcontains(output.alert_webhook_secret_ref, "?versionId=")
    error_message = "ALERT_WEBHOOK_SECRET_REF must be an awssm:// reference with a pinned versionId (internal/secretstore awssmRef)."
  }
}

run "key_family_secrets_live_under_the_boundary_prefix" {
  command = plan

  assert {
    condition = alltrue([
      for x in [
        aws_secretsmanager_secret.staff_mfa_keys,
        aws_secretsmanager_secret.payout_instrument_keys,
        aws_secretsmanager_secret.actor_proof_keys,
        aws_secretsmanager_secret.alert_webhook_hmac,
      ] : startswith(x.name, "igaming-staging/")
    ])
    error_message = "Every secret must live under igaming-staging/ (the permissions boundary and deployer policy are scoped to that prefix)."
  }
}

run "invalid_key_family_version_rejected" {
  command = plan

  variables {
    key_family_version = 0
  }

  expect_failures = [var.key_family_version]
}
