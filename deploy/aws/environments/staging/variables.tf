variable "aws_region" {
  description = "AWS region for this staging environment. eu-central-1 (Europe/Frankfurt) is the canonical staging region (ADR 0086); the remote state backend (backend.tf) lives there too."
  type        = string
  default     = "eu-central-1"

  validation {
    condition     = var.aws_region == "eu-central-1"
    error_message = "Staging is pinned to eu-central-1 (ADR 0086). Changing the staging region is a recorded decision, not a variable override."
  }
}

variable "allowed_account_ids" {
  description = "The only AWS account(s) this configuration may ever run against — the human-confirmed staging account (Stage 9.4)."
  type        = list(string)
  default     = ["765578795051"]

  validation {
    condition     = length(var.allowed_account_ids) > 0 && alltrue([for a in var.allowed_account_ids : can(regex("^[0-9]{12}$", a))])
    error_message = "allowed_account_ids must list 12-digit AWS account IDs."
  }
}

variable "name_prefix" {
  description = "Prefix applied to every resource name. Pinned: the bootstrap-created permissions boundary (igaming-staging-ecs-role-boundary), its ECR/log/secret scopes and the deployer IAM policies all use this exact prefix."
  type        = string
  default     = "igaming-staging"

  validation {
    condition     = var.name_prefix == "igaming-staging"
    error_message = "name_prefix is pinned to \"igaming-staging\": the bootstrap boundary policy and the deployer IAM policies are scoped to that exact prefix."
  }
}

# --- Access (ADR 0086) ---

variable "staging_access_cidrs" {
  description = "REQUIRED, no default. IPv4 CIDRs allowed through the CloudFront edge (everyone else gets 403) — the operator's own public IP(s) as /32 (prefix ≥ /24 enforced), plus wherever the acceptance suite runs from. Put it in the git-ignored terraform.tfvars (see terraform.tfvars.example). modules/edge re-validates the same rules."
  type        = list(string)

  validation {
    condition = length(var.staging_access_cidrs) > 0 && alltrue([
      for c in var.staging_access_cidrs :
      can(regex("^[0-9]{1,3}(\\.[0-9]{1,3}){3}/[0-9]{1,2}$", c)) && can(cidrnetmask(c)) && try(tonumber(split("/", c)[1]) >= 24, false)
    ])
    error_message = "staging_access_cidrs must list at least one IPv4 CIDR (a.b.c.d/nn) with a prefix of at least /24 — never 0.0.0.0/0 or a carrier-sized block."
  }
}

# --- Networking ---

variable "vpc_cidr" {
  type    = string
  default = "10.20.0.0/16"
}

variable "az_count" {
  type    = number
  default = 2
}

variable "ecs_public_ip_mode" {
  description = <<-EOT
    true (default, ADR 0086): no NAT Gateway; ECS tasks run in the public
    subnets with public IPs and accept traffic only from the ALB security
    group. false: the production-shaped layout — tasks in private subnets
    behind a single NAT Gateway. RDS and the internal ALB stay in the
    private subnets either way.
  EOT
  type        = bool
  default     = true
}

# --- Database (staging-sized defaults — see database module for the
#     production-promotion variables) ---

variable "db_instance_class" {
  type    = string
  default = "db.t4g.micro"
}

variable "db_allocated_storage" {
  type    = number
  default = 20
}

variable "db_engine_version" {
  description = "Pinned for eu-central-1 staging: 16.15 verified available there, with db.t4g.micro orderable, on 2026-09-23 (read-only describe-db-engine-versions / describe-orderable-db-instance-options). Re-verify before changing. This is a staging pin, not a production version policy."
  type        = string
  default     = "16.15"
}

variable "db_name" {
  type    = string
  default = "igaming_platform_staging"
}

# --- A12 / ADR 0122: managed point-in-time recovery, snapshots and audit logging for the staging database.
#     PREPARED, NOT APPLIED: nothing has been planned against AWS or applied; the single authorised staging deploy
#     follows docs/runbooks/aws-staging-deploy-plan.md. These defaults replace the previous disposable ones
#     (7 days, no deletion protection, no final snapshot): a later teardown needs the steps in that runbook.

variable "db_backup_retention_days" {
  description = "Automated backups = the PITR window, in days (1..35). 14 for staging."
  type        = number
  default     = 14
}

variable "db_backup_window" {
  description = "Daily UTC snapshot window (hh24:mi-hh24:mi); must not overlap the maintenance window."
  type        = string
  default     = "01:00-02:00"
}

variable "db_multi_az" {
  type    = bool
  default = false
}

variable "db_deletion_protection" {
  description = "true: terraform destroy / DeleteDBInstance is refused until protection is lifted on purpose (apply with -var db_deletion_protection=false first)."
  type        = bool
  default     = true
}

variable "db_skip_final_snapshot" {
  description = "false: a final snapshot is taken when the instance is destroyed."
  type        = bool
  default     = false
}

variable "db_final_snapshot_identifier" {
  description = "Final snapshot name. RDS refuses a name that already exists, so after a teardown either delete the previous snapshot (once it has served its purpose) or set a new name per deploy, e.g. igaming-staging-db-final-<short-sha>."
  type        = string
  default     = "igaming-staging-db-final"
}

variable "db_kms_key_arn" {
  description = "Reference to an EXISTING customer-managed KMS key ARN for storage/snapshot/PITR encryption. Null = the AWS-managed aws/rds key (still encrypted; ADR 0086 keeps CMKs out of the disposable staging by default). Setting it is a deploy-time decision; no key is created from this variable."
  type        = string
  default     = null
}

variable "db_log_exports" {
  description = "RDS logs exported to CloudWatch Logs (postgresql, upgrade)."
  type        = list(string)
  default     = ["postgresql", "upgrade"]
}

variable "db_enable_pgaudit" {
  description = "Add pgaudit to shared_preload_libraries (needs a reboot, then CREATE EXTENSION pgaudit; docs/runbooks/aws-staging-deploy-plan.md). AWS documents pgaudit for RDS PostgreSQL 16; not verified here."
  type        = bool
  default     = true
}

variable "backup_runner_secret_arn" {
  description = "REFERENCE ONLY: ARN of the Secrets Manager secret that will hold the igaming_backup credential (JSON keys username/password) for the backup runner. Nothing is created from it and no value is ever set by Terraform. Null until the operator creates the secret out of band at deploy time."
  type        = string
  default     = null

  validation {
    condition     = var.backup_runner_secret_arn == null || can(regex("^arn:aws:secretsmanager:eu-central-1:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]+$", var.backup_runner_secret_arn))
    error_message = "backup_runner_secret_arn must be a Secrets Manager secret ARN in eu-central-1."
  }
}

# --- Secrets ---

variable "secret_version" {
  description = "Bump to rotate the Terraform-generated runtime DB password and JWT signing secret (write-only; see modules/secrets). Then re-run role-init and redeploy services."
  type        = number
  default     = 1
}

# --- ECR / images ---

variable "image_tag" {
  description = "REQUIRED, no default. The full 40-character git commit SHA the images were built from (deploy/aws/scripts/deploy.sh passes it). ECR tags are immutable; \"latest\" and other mutable names are rejected."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f]{40}$", var.image_tag))
    error_message = "image_tag must be a full 40-character lowercase git commit SHA (immutable image identity, ADR 0086) — never \"latest\" or a short/mutable tag."
  }
}

# --- ECS sizing (staging defaults) ---

variable "platform_api_desired_count" {
  description = "1 normally (cheapest); raise to 2 only for the multi-replica acceptance test, then back to 1 (see the lifecycle runbook). The first task always runs on on-demand FARGATE; extra replicas run on FARGATE_SPOT."
  type        = number
  default     = 1

  validation {
    condition     = var.platform_api_desired_count >= 1 && var.platform_api_desired_count <= 4
    error_message = "platform_api_desired_count must be between 1 and 4 for staging."
  }
}

variable "b2c_desired_count" {
  type    = number
  default = 1
}

variable "backoffice_desired_count" {
  type    = number
  default = 1
}

variable "log_retention_days" {
  type    = number
  default = 14
}

# --- Observability ---

variable "alarm_email" {
  description = "Optional. When set, an SNS topic + email subscription are created and every alarm notifies it; when null, no SNS resources exist."
  type        = string
  default     = null
}

variable "tags" {
  type    = map(string)
  default = {}
}
