# A small set of real alarms on AWS-published (free, basic) metrics.
#
# NOTIFICATIONS (ADR 0086): the SNS topic and its email subscription exist
# only when var.alarm_email is set. With no subscriber, a topic would be
# pure no-op infrastructure, so without an email the alarms are still
# created — visible in the CloudWatch console and in `aws cloudwatch
# describe-alarms` — but have no actions.
#
# NO CONTAINER INSIGHTS DEPENDENCY (ADR 0086): Stage 9.3's "ECS running
# tasks below desired" alarms read ECS/ContainerInsights RunningTaskCount,
# which only exists when Container Insights (billed custom metrics) is
# enabled. They are replaced by per-target-group ALB HealthyHostCount
# alarms (AWS/ApplicationELB, published for every ALB at no extra charge):
# "fewer than one healthy target" catches every way a service is down —
# no running task, crash-looping tasks, or tasks failing readiness — which
# is the signal that actually matters for staging acceptance.
#
# RDS DIMENSION: AWS/RDS publishes CPUUtilization and FreeStorageSpace per
# instance under the DBInstanceIdentifier dimension (the instance's
# identifier, e.g. "igaming-staging-db"). Stage 9.3 used DbiResourceId
# (the "db-XXXX" resource ID), which those metrics are not published
# under, so both alarms could never fire and — with
# treat_missing_data = "notBreaching" — stayed silently green. Pinned by
# deploy/aws/modules/observability/tests/alarms.tftest.hcl.
#
# CloudWatch log groups themselves live in modules/ecs (colocated with the
# services that write to them) — this module only wires alarms.

locals {
  notifications_enabled = var.alarm_email != null
  alarm_actions         = local.notifications_enabled ? [aws_sns_topic.alerts[0].arn] : []
}

resource "aws_sns_topic" "alerts" {
  count = local.notifications_enabled ? 1 : 0

  name = "${var.name_prefix}-alerts"
  tags = merge(var.tags, { Name = "${var.name_prefix}-alerts" })
}

resource "aws_sns_topic_subscription" "email" {
  count = local.notifications_enabled ? 1 : 0

  topic_arn = aws_sns_topic.alerts[0].arn
  protocol  = "email"
  endpoint  = var.alarm_email
}

resource "aws_cloudwatch_metric_alarm" "alb_5xx" {
  alarm_name          = "${var.name_prefix}-alb-5xx-rate"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  period              = 60
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_Target_5XX_Count"
  statistic           = "Sum"
  threshold           = 10
  treat_missing_data  = "notBreaching"
  alarm_description   = "More than 10 target-originated 5xx responses/minute for 3 consecutive minutes."
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions

  dimensions = {
    LoadBalancer = var.alb_arn_suffix
  }

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "alb_unhealthy_hosts" {
  for_each = var.target_group_arn_suffixes

  alarm_name          = "${var.name_prefix}-${each.key}-unhealthy-hosts"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  period              = 60
  namespace           = "AWS/ApplicationELB"
  metric_name         = "UnHealthyHostCount"
  statistic           = "Maximum"
  threshold           = 0
  treat_missing_data  = "notBreaching"
  alarm_description   = "At least one unhealthy target behind the ${each.key} target group for 3 consecutive minutes."
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions

  dimensions = {
    LoadBalancer = var.alb_arn_suffix
    TargetGroup  = each.value
  }

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "alb_no_healthy_hosts" {
  for_each = var.target_group_arn_suffixes

  alarm_name          = "${var.name_prefix}-${each.key}-no-healthy-hosts"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 3
  period              = 60
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HealthyHostCount"
  statistic           = "Minimum"
  threshold           = 1
  # No data at all means the target group reports nothing healthy either —
  # fail loud, not silent.
  treat_missing_data = "breaching"
  alarm_description  = "Fewer than one healthy target behind the ${each.key} target group for 3 consecutive minutes (service down, crash-looping, or failing readiness)."
  alarm_actions      = local.alarm_actions
  ok_actions         = local.alarm_actions

  dimensions = {
    LoadBalancer = var.alb_arn_suffix
    TargetGroup  = each.value
  }

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "rds_cpu" {
  alarm_name          = "${var.name_prefix}-rds-cpu-high"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  period              = 300
  namespace           = "AWS/RDS"
  metric_name         = "CPUUtilization"
  statistic           = "Average"
  threshold           = 80
  treat_missing_data  = "notBreaching"
  alarm_description   = "RDS CPU above 80% for 15 consecutive minutes."
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions

  dimensions = {
    DBInstanceIdentifier = var.rds_instance_identifier
  }

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "rds_free_storage" {
  alarm_name          = "${var.name_prefix}-rds-free-storage-low"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 2
  period              = 300
  namespace           = "AWS/RDS"
  metric_name         = "FreeStorageSpace"
  statistic           = "Average"
  threshold           = var.rds_free_storage_threshold_bytes
  treat_missing_data  = "notBreaching"
  alarm_description   = "RDS free storage below threshold for 10 consecutive minutes."
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions

  dimensions = {
    DBInstanceIdentifier = var.rds_instance_identifier
  }

  tags = var.tags
}
