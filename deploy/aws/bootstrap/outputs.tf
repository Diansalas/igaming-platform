output "state_bucket_name" {
  value = aws_s3_bucket.state.id
}

output "state_bucket_arn" {
  value = aws_s3_bucket.state.arn
}

output "staging_ecs_role_boundary_arn" {
  value = aws_iam_policy.staging_ecs_role_boundary.arn
}
