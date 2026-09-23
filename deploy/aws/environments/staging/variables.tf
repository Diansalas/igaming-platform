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
  description = "Prefix applied to every resource name."
  type        = string
  default     = "igaming-staging"
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

variable "db_backup_retention_days" {
  type    = number
  default = 7
}

variable "db_multi_az" {
  type    = bool
  default = false
}

variable "db_deletion_protection" {
  type    = bool
  default = false
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
