variable "name_prefix" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_ids" {
  type = list(string)
}

variable "security_group_id" {
  type = string
}

variable "certificate_arn" {
  description = "ACM certificate ARN. Null (default) means no domain/Route53 zone was supplied — the ALB serves plain HTTP only on its own *.elb.amazonaws.com DNS name. Genuinely conditional: leave null to get the non-HTTPS staging fallback, set it to get a real HTTPS listener."
  type        = string
  default     = null
}

variable "container_port" {
  type    = number
  default = 8080
}

variable "api_hostname" {
  type = string
}

variable "app_hostname" {
  type = string
}

variable "admin_hostname" {
  type = string
}

variable "tags" {
  type    = map(string)
  default = {}
}
