# Three security groups forming the isolation chain described in
# docs/architecture/38-deployment-architecture.md: edge -> ALB -> ECS tasks
# -> RDS. The ALB accepts only var.alb_ingress_cidrs; ECS tasks accept
# only the ALB security group; RDS accepts only the ECS tasks security
# group. These rules are what isolate the ECS tasks even when they carry
# public IPs (the staging root's public-IP task mode, ADR 0086): a task's
# public address accepts nothing that did not come from the ALB.
#
# The ALB is internal and reached exclusively through a CloudFront VPC
# origin (ADR 0086), which connects over HTTP on port 80 (http-only). AWS
# documents exactly two ways to admit that traffic: the CloudFront
# origin-facing managed prefix list, or the service-managed
# CloudFront-VPCOrigins-Service-SG. Admitting the VPC-origin ENIs' subnet
# CIDRs is NOT one of them: with CIDR-only ingress the first staging
# deployment got CloudFront 504s and the ALB received zero requests. The
# prefix list is used because it exists before the VPC origin does, so a
# single apply works (the service-managed SG is only created afterwards).
# No 443 rule: nothing reaches this ALB over HTTPS, and each prefix-list
# reference counts its list's weight against the per-SG rule quota.

data "aws_ec2_managed_prefix_list" "cloudfront_origin_facing" {
  name = "com.amazonaws.global.cloudfront.origin-facing"
}

resource "aws_security_group" "alb" {
  name = "${var.name_prefix}-alb-sg"
  # Stale wording kept on purpose: description is ForceNew, and replacing
  # this SG would also replace the rules that reference it. The ingress
  # below is authoritative.
  description = "Ingress on 80/443 from alb_ingress_cidrs only; egress to ECS tasks."
  vpc_id      = var.vpc_id

  ingress {
    description     = "HTTP from CloudFront VPC origin (origin-facing prefix list)"
    from_port       = 80
    to_port         = 80
    protocol        = "tcp"
    prefix_list_ids = [data.aws_ec2_managed_prefix_list.cloudfront_origin_facing.id]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-alb-sg" })
}

resource "aws_security_group" "ecs_tasks" {
  name        = "${var.name_prefix}-ecs-tasks-sg"
  description = "Ingress only from the ALB security group on the container port; egress to RDS and to ECR/Secrets Manager/CloudWatch Logs (via NAT or the task public IP)."
  vpc_id      = var.vpc_id

  ingress {
    description     = "From ALB"
    from_port       = var.container_port
    to_port         = var.container_port
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-ecs-tasks-sg" })
}

resource "aws_security_group" "rds" {
  name        = "${var.name_prefix}-rds-sg"
  description = "Ingress only from the ECS tasks security group on the Postgres port."
  vpc_id      = var.vpc_id

  ingress {
    description     = "From ECS tasks"
    from_port       = var.db_port
    to_port         = var.db_port
    protocol        = "tcp"
    security_groups = [aws_security_group.ecs_tasks.id]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-rds-sg" })
}
