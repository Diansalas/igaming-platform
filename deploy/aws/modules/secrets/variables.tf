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
