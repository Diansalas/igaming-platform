output "sns_topic_arn" {
  description = "Null when alarm_email is not set (no SNS resources are created)."
  value       = one(aws_sns_topic.alerts[*].arn)
}
