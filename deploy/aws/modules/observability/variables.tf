variable "name_prefix" {
  type = string
}

variable "alb_arn_suffix" {
  type = string
}

variable "target_group_arn_suffixes" {
  description = "map(service_name => arn_suffix), used for per-target-group unhealthy-host alarms."
  type        = map(string)
}

variable "rds_resource_id" {
  type = string
}

variable "ecs_cluster_name" {
  type = string
}

variable "ecs_services" {
  description = "map(service_name => desired_count) — used for the running-task-count-below-desired alarm."
  type        = map(number)
}

variable "alarm_email" {
  description = "Optional email address subscribed to the SNS alarm topic. Staging does not require this to be set."
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
