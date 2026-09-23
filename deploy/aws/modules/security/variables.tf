variable "name_prefix" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "container_port" {
  description = "Container port every ECS service (platform-api, b2c, backoffice) listens on."
  type        = number
  default     = 8080
}

variable "db_port" {
  type    = number
  default = 5432
}

variable "alb_ingress_cidrs" {
  description = "CIDRs allowed to reach the ALB on 80/443. The module default is open to the internet (a public ALB); the staging root overrides it with the VPC CIDR only, because its ALB is internal and fronted by CloudFront VPC origins (ADR 0086)."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "tags" {
  type    = map(string)
  default = {}
}
