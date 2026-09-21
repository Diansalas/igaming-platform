variable "aws_region" {
  description = "AWS region for this staging environment."
  type        = string
  default     = "eu-west-1"
}

variable "name_prefix" {
  description = "Prefix applied to every resource name."
  type        = string
  default     = "igaming-staging"
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
  type    = string
  default = "16.4"
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

# --- ECR / images ---

variable "image_tag" {
  description = "Image tag to deploy for all 3 services. deploy/aws/scripts/deploy.sh pushes and then applies with this set (commonly a git SHA)."
  type        = string
  default     = "latest"
}

# --- ECS sizing (staging defaults) ---

variable "platform_api_desired_count" {
  type    = number
  default = 2
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

# --- Domain / TLS (genuinely optional — leave both null for the
#     non-HTTPS ALB-DNS-name fallback) ---

variable "domain_name" {
  description = "Apex domain to build the 3 staging hostnames under (api-staging.<domain>, app-staging.<domain>, admin-staging.<domain>). Null = no custom domain; the ALB falls back to plain HTTP on its own *.elb.amazonaws.com DNS name."
  type        = string
  default     = null
}

variable "route53_zone_id" {
  description = "Route53 hosted zone ID for domain_name. Required together with domain_name to get a real HTTPS listener + DNS records; either both are set or neither is."
  type        = string
  default     = null
}

# --- Observability ---

variable "alarm_email" {
  description = "Optional email address subscribed to the CloudWatch alarm SNS topic. Staging does not require this to be set."
  type        = string
  default     = null
}

variable "tags" {
  type    = map(string)
  default = {}
}
