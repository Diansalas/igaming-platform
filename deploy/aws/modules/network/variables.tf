variable "name_prefix" {
  description = "Prefix applied to every resource name created by this module."
  type        = string
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.20.0.0/16"
}

variable "az_count" {
  description = "Number of availability zones to spread subnets across. Staging uses 2; this is the minimum for any meaningful HA story and matches the deployment architecture's 'at least two replicas' requirement."
  type        = number
  default     = 2
}

variable "public_subnet_cidrs" {
  description = "CIDR blocks for public subnets, one per AZ (internet-facing load balancers, the optional NAT Gateway, and — in the staging root's public-IP task mode — the ECS tasks themselves)."
  type        = list(string)
  default     = ["10.20.0.0/20", "10.20.16.0/20"]
}

variable "private_subnet_cidrs" {
  description = "CIDR blocks for private subnets, one per AZ (RDS and internal load balancers; ECS tasks too when NAT is enabled). No public IPs; an outbound internet route only when enable_nat_gateway = true."
  type        = list(string)
  default     = ["10.20.128.0/20", "10.20.144.0/20"]
}

variable "enable_nat_gateway" {
  description = "Create a single shared NAT Gateway (plus its Elastic IP) and route the private subnets' outbound traffic through it. Default true (production-shaped). The staging root sets false — see ADR 0086."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Extra tags merged onto every resource."
  type        = map(string)
  default     = {}
}
