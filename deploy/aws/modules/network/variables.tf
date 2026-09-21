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
  description = "CIDR blocks for public subnets, one per AZ (ALB lives here)."
  type        = list(string)
  default     = ["10.20.0.0/20", "10.20.16.0/20"]
}

variable "private_subnet_cidrs" {
  description = "CIDR blocks for private subnets, one per AZ (ECS tasks + RDS live here, no public IP/route)."
  type        = list(string)
  default     = ["10.20.128.0/20", "10.20.144.0/20"]
}

variable "tags" {
  description = "Extra tags merged onto every resource."
  type        = map(string)
  default     = {}
}
