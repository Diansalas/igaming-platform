variable "name_prefix" {
  type = string
}

variable "db_runtime_username" {
  type    = string
  default = "igaming_runtime"
}

variable "secret_version" {
  description = "Write-only version counter for every generated secret value in this module. Terraform writes a newly generated value only when this changes — bump it to rotate (then re-run role-init and redeploy services; see the staging runbook)."
  type        = number
  default     = 1

  validation {
    condition     = var.secret_version >= 1 && floor(var.secret_version) == var.secret_version
    error_message = "secret_version must be a positive integer."
  }
}

variable "key_family_version" {
  description = "Write-only version counter for the generated key-family secrets (staff MFA, payout instrument, actor proof, alert webhook HMAC). Also embedded in the kid (stg-v<N>). Separate from secret_version so rotating the database password or the JWT secret does not invalidate enrolled MFA factors or sealed payout instruments. This generator does not keep the previous key: see the comment in main.tf."
  type        = number
  default     = 1

  validation {
    condition     = var.key_family_version >= 1 && floor(var.key_family_version) == var.key_family_version
    error_message = "key_family_version must be a positive integer."
  }
}

variable "seed_admin_secret_version" {
  description = "Write-only version counter for the seed-admin password only (NOT bumped by secret_version: the admin's stored password hash would not follow). Bump only together with seeding a new admin in a fresh environment."
  type        = number
  default     = 1

  validation {
    condition     = var.seed_admin_secret_version >= 1 && floor(var.seed_admin_secret_version) == var.seed_admin_secret_version
    error_message = "seed_admin_secret_version must be a positive integer."
  }
}

variable "recovery_window_in_days" {
  description = <<-EOT
    Secrets Manager recovery window applied when these secrets are deleted
    (e.g. by terraform destroy). Default 30 — the AWS default and the
    production-safe choice: a deleted secret stays recoverable for 30 days.
    0 means FORCE DELETE with no recovery window: the secret is gone
    immediately and its name can be reused at once. Only a disposable
    environment should set 0 — the staging root does, so a destroyed
    staging environment can be re-created immediately instead of failing on
    "a secret with this name is already scheduled for deletion" (ADR 0086).
  EOT
  type        = number
  default     = 30

  validation {
    condition     = var.recovery_window_in_days == 0 || (var.recovery_window_in_days >= 7 && var.recovery_window_in_days <= 30)
    error_message = "recovery_window_in_days must be 0 (force delete, disposable environments only) or between 7 and 30."
  }
}

variable "tags" {
  type    = map(string)
  default = {}
}
