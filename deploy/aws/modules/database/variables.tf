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
  description = "Automated backup retention in days = the point-in-time-recovery window (RDS PITR restores to any second inside it, up to latest_restorable_time). The staging root sets 14 (A12 / ADR 0122); production should use a longer window. 0 would disable automated backups AND PITR and is rejected."
  type        = number
  default     = 7

  validation {
    condition     = var.backup_retention_days >= 1 && var.backup_retention_days <= 35
    error_message = "backup_retention_days must be between 1 and 35 (RDS limit); 0 would disable automated backups and point-in-time recovery."
  }
}

variable "multi_az" {
  description = "Staging default false (single AZ, cost-optimized). Production must set true."
  type        = bool
  default     = false
}

variable "deletion_protection" {
  description = "Module default false keeps the legacy disposable behaviour. The staging root sets true (A12): terraform destroy then fails until protection is lifted deliberately (docs/runbooks/aws-staging-deploy-plan.md). Production must set true."
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

# --- A12 / ADR 0122: managed point-in-time recovery (PITR), snapshots and audit logging.
#     PREPARED, NOT APPLIED: no terraform apply has been run for these settings and none is authorised until the single
#     named-commit staging deploy (docs/runbooks/aws-staging-deploy-plan.md).

variable "backup_window" {
  description = "Daily UTC window for the automated snapshot (hh24:mi-hh24:mi, at least 30 minutes). Must not overlap maintenance_window. PITR itself is continuous (transaction logs are shipped to S3 about every 5 minutes) and does not depend on this window."
  type        = string
  default     = "01:00-02:00"

  validation {
    condition     = can(regex("^([01][0-9]|2[0-3]):[0-5][0-9]-([01][0-9]|2[0-3]):[0-5][0-9]$", var.backup_window))
    error_message = "backup_window must look like 01:00-02:00 (UTC)."
  }
}

variable "maintenance_window" {
  description = "Weekly UTC maintenance window (ddd:hh24:mi-ddd:hh24:mi). Must not overlap backup_window."
  type        = string
  default     = "sun:04:30-sun:05:30"

  validation {
    condition     = can(regex("^(mon|tue|wed|thu|fri|sat|sun):[0-2][0-9]:[0-5][0-9]-(mon|tue|wed|thu|fri|sat|sun):[0-2][0-9]:[0-5][0-9]$", var.maintenance_window))
    error_message = "maintenance_window must look like sun:04:30-sun:05:30 (UTC)."
  }
}

variable "skip_final_snapshot" {
  description = "true (module default, keeps the legacy disposable behaviour) skips the final snapshot on destroy. The staging root sets false (A12): a destroy then takes a final snapshot named final_snapshot_identifier. When false, final_snapshot_identifier is REQUIRED (precondition)."
  type        = bool
  default     = true
}

variable "final_snapshot_identifier" {
  description = "Name of the final snapshot taken on destroy when skip_final_snapshot = false. It must be unique per destroy (RDS refuses an existing name): include the deploy commit or date, e.g. igaming-staging-db-final-<short-sha>. Null while skip_final_snapshot = true."
  type        = string
  default     = null

  validation {
    condition     = var.final_snapshot_identifier == null || can(regex("^[a-z][a-z0-9-]{0,254}$", var.final_snapshot_identifier))
    error_message = "final_snapshot_identifier must be lowercase letters, digits and hyphens, starting with a letter."
  }
}

variable "kms_key_arn" {
  description = "ARN of an EXISTING customer-managed KMS key for storage (and therefore snapshot / PITR) encryption. When set it takes precedence over create_kms_key. Null keeps the previous behaviour (module-created CMK or the AWS-managed aws/rds key). A reference only: no key is created from this variable."
  type        = string
  default     = null

  validation {
    condition     = var.kms_key_arn == null || can(regex("^arn:aws:kms:[a-z0-9-]+:[0-9]{12}:key/[0-9a-f-]{36}$", var.kms_key_arn))
    error_message = "kms_key_arn must be a KMS key ARN (arn:aws:kms:<region>:<account>:key/<uuid>)."
  }
}

variable "enabled_cloudwatch_logs_exports" {
  description = "RDS log types exported to CloudWatch Logs. Valid for PostgreSQL: postgresql, upgrade. The staging root enables both so connection / disconnection / pgaudit lines of the backup role are retained off-instance."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for l in var.enabled_cloudwatch_logs_exports : contains(["postgresql", "upgrade"], l)])
    error_message = "enabled_cloudwatch_logs_exports may only contain postgresql and upgrade."
  }
}

variable "log_retention_days" {
  description = "Retention (days) of the pre-created CloudWatch log groups for the exported RDS logs. Must be a value CloudWatch accepts."
  type        = number
  default     = 30

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.log_retention_days)
    error_message = "log_retention_days must be a retention value CloudWatch Logs accepts."
  }
}

variable "enable_pgaudit" {
  description = "Add pgaudit to shared_preload_libraries (static parameter: takes effect at the next reboot; an operator then runs CREATE EXTENSION pgaudit and sets pgaudit.log per role, see ADR 0122). Engine support for pgaudit on RDS PostgreSQL 16 is AWS-documented but NOT verified by this repository."
  type        = bool
  default     = false
}
