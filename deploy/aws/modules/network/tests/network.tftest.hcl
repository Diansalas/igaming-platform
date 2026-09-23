mock_provider "aws" {
  mock_data "aws_availability_zones" {
    defaults = {
      names = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
    }
  }
}

variables {
  name_prefix = "igaming-staging"
}

run "nat_enabled_by_default" {
  command = plan

  assert {
    condition     = length(aws_nat_gateway.this) == 1 && length(aws_eip.nat) == 1 && length(aws_route.private_nat) == 1
    error_message = "Module default (production-shaped) must create the NAT Gateway, its EIP and the private default route."
  }
}

run "nat_disabled" {
  command = plan

  variables {
    enable_nat_gateway = false
  }

  assert {
    condition     = length(aws_nat_gateway.this) == 0 && length(aws_eip.nat) == 0 && length(aws_route.private_nat) == 0
    error_message = "enable_nat_gateway = false must create no NAT Gateway, no EIP and no private default route."
  }

  assert {
    condition     = alltrue([for s in aws_subnet.private : s.map_public_ip_on_launch == false])
    error_message = "Private subnets must never auto-assign public IPs."
  }
}
