variable "name_prefix" {
  type = string
}

variable "alb_arn_suffix" {
  type = string
}

variable "target_group_arn_suffixes" {
  description = "map(service_name => arn_suffix), used for per-target-group healthy/unhealthy-host alarms."
  type        = map(string)
}

variable "rds_instance_identifier" {
  description = "The RDS instance IDENTIFIER (e.g. \"igaming-staging-db\") — the DBInstanceIdentifier dimension AWS/RDS publishes CPUUtilization/FreeStorageSpace under. NOT the \"db-XXXX\" DbiResourceId."
  type        = string

  validation {
    condition     = !can(regex("^db-[A-Z0-9]+$", var.rds_instance_identifier))
    error_message = "rds_instance_identifier looks like a DbiResourceId (db-XXXX). Pass the instance identifier (aws_db_instance.identifier) instead — the AWS/RDS alarm metrics are published under DBInstanceIdentifier."
  }
}

variable "alarm_email" {
  description = "Optional email address. When set, an SNS topic + email subscription are created and every alarm notifies it; when null (default), no SNS resources exist and alarms have no actions."
  type        = string
  default     = null
}

variable "rds_free_storage_threshold_bytes" {
  description = "Alarm when RDS free storage drops below this many bytes."
  type        = number
  default     = 2147483648 # 2 GiB
}

variable "tags" {
  type    = map(string)
  default = {}
}
