# Three security groups forming the isolation chain described in
# docs/architecture/38-deployment-architecture.md: edge -> ALB -> ECS tasks
# -> RDS. The ALB accepts only var.alb_ingress_cidrs; ECS tasks accept
# only the ALB security group; RDS accepts only the ECS tasks security
# group. These rules are what isolate the ECS tasks even when they carry
# public IPs (the staging root's public-IP task mode, ADR 0086): a task's
# public address accepts nothing that did not come from the ALB.
#
# The staging root sets alb_ingress_cidrs to the VPC CIDR only: its ALB is
# internal and is reached exclusively through CloudFront VPC origins,
# whose elastic network interfaces live inside the VPC (ADR 0086).

resource "aws_security_group" "alb" {
  name        = "${var.name_prefix}-alb-sg"
  description = "Ingress on 80/443 from alb_ingress_cidrs only; egress to ECS tasks."
  vpc_id      = var.vpc_id

  ingress {
    description = "HTTP"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = var.alb_ingress_cidrs
  }

  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = var.alb_ingress_cidrs
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
