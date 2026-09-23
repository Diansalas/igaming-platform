variable "aws_region" {
  type    = string
  default = "eu-central-1"

  validation {
    condition     = var.aws_region == "eu-central-1"
    error_message = "The staging state bucket lives in eu-central-1 (ADR 0086)."
  }
}

variable "allowed_account_ids" {
  type    = list(string)
  default = ["765578795051"]
}

variable "state_bucket_name" {
  description = "Must equal the bucket in deploy/aws/environments/staging/backend.tf."
  type        = string
  default     = "igaming-platform-staging-tfstate-765578795051"
}

variable "noncurrent_version_retention_days" {
  description = "How long superseded state versions are kept (recovery window for a bad apply)."
  type        = number
  default     = 90
}

variable "budget_alert_email" {
  description = "Optional. When set, creates a monthly AWS cost budget for the whole account that emails this address at 50%/80%/100% of actual spend and at 100% of forecast — the backstop against a staging environment accidentally left running."
  type        = string
  default     = null
}

variable "monthly_budget_usd" {
  description = "Monthly account cost budget (USD) for the alert above."
  type        = number
  default     = 25
}
