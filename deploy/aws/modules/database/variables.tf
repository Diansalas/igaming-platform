variable "name_prefix" {
  type = string
}

variable "private_subnet_ids" {
  type = list(string)
}

variable "security_group_id" {
  type = string
}

variable "instance_class" {
  description = "Staging-sized default. Bump for production (see runbook's promotion section)."
  type        = string
  default     = "db.t4g.micro"
}

variable "allocated_storage" {
  type    = number
  default = 20
}

variable "engine_version" {
  description = "Exact Postgres engine version. Deliberately has NO module default: each environment root pins its own, verified against `aws rds describe-db-engine-versions --engine postgres --region <region>` for that region (minor versions are retired over time, so a stale default fails only at apply time — this is exactly what happened to the Stage 9.3 default of 16.4 in eu-central-1)."
  type        = string

  validation {
    condition     = can(regex("^16\\.[0-9]+$", var.engine_version))
    error_message = "engine_version must be an exact PostgreSQL 16 minor version such as \"16.15\" (the parameter group family is postgres16)."
  }
}

variable "master_username" {
  description = "RDS master user name. Named 'igaming' deliberately: an RDS master user already owns the initial database/schema it creates and holds the rds_superuser pseudo-role (CREATEROLE, no true SUPERUSER) — this IS the migration-owner role from docs/security/runtime-role-separation.md, not a separate bootstrap identity. cmd/migrate connects as this role."
  type        = string
  default     = "igaming"
}

variable "db_name" {
  type    = string
  default = "igaming_platform_staging"
}

variable "backup_retention_days" {
  description = "Automated backup retention. Short for staging (3-7 days recommended); production should use a longer window."
  type        = number
  default     = 7

  validation {
    condition     = var.backup_retention_days >= 1 && var.backup_retention_days <= 35
    error_message = "backup_retention_days must be between 1 and 35 (RDS limit)."
  }
}

variable "multi_az" {
  description = "Staging default false (single AZ, cost-optimized). Production must set true."
  type        = bool
  default     = false
}

variable "deletion_protection" {
  description = "Staging default false so the environment is fully disposable via terraform destroy. Production must set true."
  type        = bool
  default     = false
}

variable "auto_minor_version_upgrade" {
  description = "Let RDS apply minor engine upgrades in the maintenance window. Default true; a root that pins an exact engine_version (staging) sets false so Terraform and RDS never disagree about the version."
  type        = bool
  default     = true
}

variable "create_kms_key" {
  description = "Create a dedicated customer-managed KMS key for storage encryption (default true). false uses the AWS-managed aws/rds key — storage is encrypted either way. The staging root sets false (ADR 0086)."
  type        = bool
  default     = true
}

variable "kms_deletion_window_days" {
  type    = number
  default = 7
}

variable "tags" {
  type    = map(string)
  default = {}
}
