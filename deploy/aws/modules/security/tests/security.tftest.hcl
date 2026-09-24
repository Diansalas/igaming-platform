# Pins the security-group isolation chain (QA review P0): the ALB accepts
# only the CloudFront origin-facing managed prefix list on port 80 (the
# documented CloudFront VPC-origin source); ECS tasks accept only the ALB
# security group; RDS accepts only the ECS tasks security group — never
# CIDR-based access to the ALB, the tasks or the database.
#
# Regression: CIDR-based ALB ingress (the private subnets, where the
# VPC-origin ENIs live) made every CloudFront request time out with 504 on
# the first real staging deployment — the ALB received zero requests.

mock_provider "aws" {
  mock_data "aws_ec2_managed_prefix_list" {
    defaults = {
      id = "pl-a3a144ca"
    }
  }
}

# (Unset list attributes are null under the mock provider, hence coalesce.)

variables {
  name_prefix = "igaming-staging"
  vpc_id      = "vpc-0aaaaaaaaaaaaaaa1"
}

run "isolation_chain" {
  command = apply

  assert {
    condition     = data.aws_ec2_managed_prefix_list.cloudfront_origin_facing.name == "com.amazonaws.global.cloudfront.origin-facing"
    error_message = "The ALB ingress prefix list must be resolved by the CloudFront origin-facing managed prefix list name."
  }

  assert {
    condition = length(aws_security_group.alb.ingress) == 1 && alltrue([
      for r in aws_security_group.alb.ingress :
      r.from_port == 80 && r.to_port == 80 && r.protocol == "tcp" && toset(r.prefix_list_ids) == toset(["pl-a3a144ca"])
    ])
    error_message = "The ALB must have exactly one ingress rule: tcp/80 from the CloudFront origin-facing prefix list (the VPC origin is http-only; no 443 rule)."
  }

  assert {
    condition = alltrue([
      for r in aws_security_group.alb.ingress :
      length(coalesce(r.cidr_blocks, [])) == 0 && length(coalesce(r.ipv6_cidr_blocks, [])) == 0 && length(coalesce(r.security_groups, [])) == 0 && !coalesce(r.self, false)
    ])
    error_message = "ALB ingress must not use CIDRs (subnet, VPC or 0.0.0.0/0), IPv6 CIDRs, other security groups or self — only the CloudFront prefix list."
  }

  assert {
    condition = alltrue([
      for r in aws_security_group.ecs_tasks.ingress :
      length(coalesce(r.cidr_blocks, [])) == 0 && length(coalesce(r.ipv6_cidr_blocks, [])) == 0 && length(coalesce(r.prefix_list_ids, [])) == 0 && r.security_groups == toset([aws_security_group.alb.id]) && r.from_port == 8080 && r.to_port == 8080
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
