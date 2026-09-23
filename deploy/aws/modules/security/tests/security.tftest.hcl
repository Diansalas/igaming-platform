# Pins the security-group isolation chain (QA review P0): ALB accepts only
# alb_ingress_cidrs; ECS tasks accept only the ALB security group; RDS
# accepts only the ECS tasks security group — never CIDR-based access to
# the tasks or the database.

mock_provider "aws" {}

# (Unset list attributes are null under the mock provider, hence coalesce.)

variables {
  name_prefix       = "igaming-staging"
  vpc_id            = "vpc-0aaaaaaaaaaaaaaa1"
  alb_ingress_cidrs = ["10.20.128.0/20", "10.20.144.0/20"]
}

run "isolation_chain" {
  command = apply

  assert {
    condition     = alltrue([for r in aws_security_group.alb.ingress : toset(r.cidr_blocks) == toset(var.alb_ingress_cidrs) && length(coalesce(r.security_groups, [])) == 0])
    error_message = "ALB ingress must be exactly alb_ingress_cidrs."
  }

  assert {
    condition = alltrue([
      for r in aws_security_group.ecs_tasks.ingress :
      length(coalesce(r.cidr_blocks, [])) == 0 && length(coalesce(r.ipv6_cidr_blocks, [])) == 0 && r.security_groups == toset([aws_security_group.alb.id]) && r.from_port == 8080 && r.to_port == 8080
    ]) && length(aws_security_group.ecs_tasks.ingress) == 1
    error_message = "ECS tasks must accept ONLY the ALB security group on the container port — even with public IPs."
  }

  assert {
    condition = alltrue([
      for r in aws_security_group.rds.ingress :
      length(coalesce(r.cidr_blocks, [])) == 0 && length(coalesce(r.ipv6_cidr_blocks, [])) == 0 && r.security_groups == toset([aws_security_group.ecs_tasks.id]) && r.from_port == 5432
    ]) && length(aws_security_group.rds.ingress) == 1
    error_message = "RDS must accept ONLY the ECS tasks security group on 5432."
  }
}
