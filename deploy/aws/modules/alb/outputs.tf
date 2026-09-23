output "alb_arn" {
  value = aws_lb.this.arn
}

output "alb_arn_suffix" {
  value = aws_lb.this.arn_suffix
}

output "dns_name" {
  value = aws_lb.this.dns_name
}

output "zone_id" {
  value = aws_lb.this.zone_id
}

output "https_enabled" {
  value = local.https_enabled
}

output "target_group_arns" {
  value = {
    platform_api = aws_lb_target_group.platform_api.arn
    b2c          = aws_lb_target_group.b2c.arn
    backoffice   = aws_lb_target_group.backoffice.arn
  }
}

output "target_group_arn_suffixes" {
  value = {
    platform_api = aws_lb_target_group.platform_api.arn_suffix
    b2c          = aws_lb_target_group.b2c.arn_suffix
    backoffice   = aws_lb_target_group.backoffice.arn_suffix
  }
}

output "internal" {
  value = aws_lb.this.internal
}
