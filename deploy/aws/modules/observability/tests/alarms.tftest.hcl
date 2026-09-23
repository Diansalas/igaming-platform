# Pins the Stage 9.4 RDS alarm fix (ADR 0086): AWS/RDS CPUUtilization and
# FreeStorageSpace are published under DBInstanceIdentifier; alarms on
# DbiResourceId never fire. Also pins conditional SNS. Offline (mock AWS).

mock_provider "aws" {
  mock_resource "aws_sns_topic" {
    defaults = {
      arn = "arn:aws:sns:eu-central-1:765578795051:igaming-staging-alerts"
    }
  }
}

variables {
  name_prefix    = "igaming-staging"
  alb_arn_suffix = "app/igaming-staging-alb/0123456789abcdef"
  target_group_arn_suffixes = {
    platform_api = "targetgroup/igaming-staging-api-tg/0123456789abcdef"
    b2c          = "targetgroup/igaming-staging-b2c-tg/0123456789abcdef"
    backoffice   = "targetgroup/igaming-staging-backoffice-tg/0123456789abcdef"
  }
  rds_instance_identifier = "igaming-staging-db"
}

run "rds_alarms_use_db_instance_identifier" {
  command = plan

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.rds_cpu.dimensions) == 1 && lookup(aws_cloudwatch_metric_alarm.rds_cpu.dimensions, "DBInstanceIdentifier", "") == "igaming-staging-db"
    error_message = "rds_cpu must be dimensioned ONLY by DBInstanceIdentifier = the instance identifier."
  }

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.rds_free_storage.dimensions) == 1 && lookup(aws_cloudwatch_metric_alarm.rds_free_storage.dimensions, "DBInstanceIdentifier", "") == "igaming-staging-db"
    error_message = "rds_free_storage must be dimensioned ONLY by DBInstanceIdentifier = the instance identifier."
  }

  assert {
    condition     = aws_cloudwatch_metric_alarm.rds_cpu.namespace == "AWS/RDS" && aws_cloudwatch_metric_alarm.rds_free_storage.namespace == "AWS/RDS"
    error_message = "RDS alarms must read the AWS/RDS namespace."
  }
}

run "resource_id_shaped_identifier_is_rejected" {
  command = plan

  variables {
    rds_instance_identifier = "db-ABCDEFGHIJKLMNOPQRSTUVWXY2"
  }

  expect_failures = [var.rds_instance_identifier]
}

run "no_container_insights_dependency" {
  command = plan

  assert {
    condition     = alltrue([for a in aws_cloudwatch_metric_alarm.alb_no_healthy_hosts : a.namespace == "AWS/ApplicationELB" && a.metric_name == "HealthyHostCount"])
    error_message = "Service-down alarms must use the free AWS/ApplicationELB HealthyHostCount metric, not ECS/ContainerInsights."
  }

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.alb_no_healthy_hosts) == 3
    error_message = "One no-healthy-hosts alarm per target group."
  }
}

run "no_sns_without_email" {
  command = plan

  assert {
    condition     = length(aws_sns_topic.alerts) == 0 && length(aws_sns_topic_subscription.email) == 0
    error_message = "No SNS topic/subscription may exist when alarm_email is unset."
  }

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.rds_cpu.alarm_actions) == 0
    error_message = "Alarms must have no actions when notifications are disabled."
  }
}

run "sns_with_email" {
  command = apply

  variables {
    alarm_email = "oncall@example.com"
  }

  assert {
    condition     = length(aws_sns_topic.alerts) == 1 && length(aws_sns_topic_subscription.email) == 1
    error_message = "alarm_email must create exactly one topic and one subscription."
  }

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.rds_cpu.alarm_actions) == 1 && contains(aws_cloudwatch_metric_alarm.rds_cpu.alarm_actions, aws_sns_topic.alerts[0].arn)
    error_message = "Alarms must notify the topic when alarm_email is set."
  }
}
