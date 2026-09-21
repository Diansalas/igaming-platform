# A small number of real alarms wired to an optional SNS topic (email
# endpoint is a variable; staging does not require it to be set — the
# alarms and topic still exist so they're wired correctly and ready to be
# subscribed to the moment an operator supplies alarm_email).
#
# CloudWatch log groups themselves live in modules/ecs (colocated with the
# services that write to them) — this module only wires alarms.

resource "aws_sns_topic" "alerts" {
  name = "${var.name_prefix}-alerts"
  tags = merge(var.tags, { Name = "${var.name_prefix}-alerts" })
}

resource "aws_sns_topic_subscription" "email" {
  count     = var.alarm_email != null ? 1 : 0
  topic_arn = aws_sns_topic.alerts.arn
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
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]

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
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]

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
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]

  dimensions = {
    DbiResourceId = var.rds_resource_id
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
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]

  dimensions = {
    DbiResourceId = var.rds_resource_id
  }

  tags = var.tags
}

# Requires Container Insights enabled on the ECS cluster (set in
# modules/ecs) — ECS/ContainerInsights publishes RunningTaskCount per
# service, which we compare against each service's own desired count.
resource "aws_cloudwatch_metric_alarm" "ecs_running_tasks_low" {
  for_each = var.ecs_services

  alarm_name          = "${var.name_prefix}-${each.key}-running-tasks-low"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 3
  period              = 60
  namespace           = "ECS/ContainerInsights"
  metric_name         = "RunningTaskCount"
  statistic           = "Average"
  threshold           = each.value
  treat_missing_data  = "breaching"
  alarm_description   = "Fewer than ${each.value} running task(s) for service ${each.key} for 3 consecutive minutes."
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]

  dimensions = {
    ClusterName = var.ecs_cluster_name
    ServiceName = "${var.name_prefix}-${each.key}"
  }

  tags = var.tags
}
