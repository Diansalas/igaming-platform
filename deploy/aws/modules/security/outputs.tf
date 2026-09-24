output "alb_security_group_id" {
  value = aws_security_group.alb.id
}

output "ecs_security_group_id" {
  value = aws_security_group.ecs_tasks.id
}

output "rds_security_group_id" {
  value = aws_security_group.rds.id
}

output "alb_ingress_prefix_list_id" {
  description = "The CloudFront origin-facing managed prefix list the ALB admits on port 80."
  value       = data.aws_ec2_managed_prefix_list.cloudfront_origin_facing.id
}
