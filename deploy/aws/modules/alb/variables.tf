variable "name_prefix" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "internal" {
  description = "Create an internal (VPC-only, no public IPs) load balancer. Default false (public). The staging root sets true: its ALB is reached only through CloudFront VPC origins (ADR 0086)."
  type        = bool
  default     = false
}

variable "subnet_ids" {
  description = "Subnets for the load balancer (at least two AZs): public subnets for a public ALB, private subnets for an internal one."
  type        = list(string)
}

variable "security_group_id" {
  type = string
}

variable "certificate_arn" {
  description = "ACM certificate ARN. Null (default) means no HTTPS listener on the ALB itself. Genuinely conditional: leave null for the plain-HTTP listener only, set it to get a real HTTPS listener. The staging root leaves it null because TLS terminates at CloudFront instead (ADR 0086)."
  type        = string
  default     = null
}

variable "routing_header_name" {
  description = "When set, listener rules route on this request header (values platform-api / b2c / backoffice) instead of the Host header, and unmatched requests get a 404. Used when CloudFront fronts the ALB (ADR 0086). Null (default) = host routing."
  type        = string
  default     = null
}

variable "container_port" {
  type    = number
  default = 8080
}

variable "api_hostname" {
  description = "Host-routing mode only."
  type        = string
  default     = null
}

variable "app_hostname" {
  description = "Host-routing mode only."
  type        = string
  default     = null
}

variable "admin_hostname" {
  description = "Host-routing mode only."
  type        = string
  default     = null
}

variable "tags" {
  type    = map(string)
  default = {}
}
