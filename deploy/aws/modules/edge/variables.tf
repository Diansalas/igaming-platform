variable "name_prefix" {
  type = string
}

variable "alb_arn" {
  description = "ARN of the INTERNAL ALB the VPC origin targets."
  type        = string
}

variable "alb_dns_name" {
  type = string
}

variable "routing_header_name" {
  description = "Origin custom header carrying the service key; must match modules/alb's routing_header_name."
  type        = string
}

variable "allowed_viewer_cidrs" {
  description = <<-EOT
    IPv4 CIDRs allowed to reach the staging distributions (everyone else
    gets 403 at the edge). Must be supplied explicitly — there is no default
    — and must include every place a browser or the acceptance suite will
    connect from. Prefixes shorter than /16 are rejected so this can never
    silently become "the whole internet".
  EOT
  type        = list(string)

  validation {
    condition     = length(var.allowed_viewer_cidrs) > 0
    error_message = "allowed_viewer_cidrs must contain at least one IPv4 CIDR."
  }

  validation {
    condition = alltrue([
      for c in var.allowed_viewer_cidrs :
      can(cidrnetmask(c)) && can(regex("^[0-9.]+/[0-9]+$", c)) && tonumber(split("/", c)[1]) >= 16
    ])
    error_message = "Each allowed_viewer_cidrs entry must be an IPv4 CIDR (a.b.c.d/nn) with a prefix length of at least /16."
  }
}

variable "price_class" {
  description = "PriceClass_100 = North America + Europe edge locations only (the cheapest class)."
  type        = string
  default     = "PriceClass_100"
}

variable "tags" {
  type    = map(string)
  default = {}
}
