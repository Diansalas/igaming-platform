# ONE-TIME bootstrap for the staging Terraform remote state (ADR 0086).
# Applied once, by a human-authorized deployment credential, BEFORE the
# first `terraform init` of deploy/aws/environments/staging. It is NOT part
# of the create/destroy staging lifecycle and is never destroyed with it.
#
# Creates:
#   - the IAM permissions boundary every staging ECS role must carry
#     (igaming-staging-ecs-role-boundary). The staging deployer policy only
#     allows creating/editing roles with exactly this boundary, which is what
#     stops a deployer credential from minting an admin role (ADR 0086,
#     security review P1). It is created here, by the human-run bootstrap,
#     precisely so the deployer credential itself never needs permission to
#     create or edit it.
#   - the S3 state bucket: versioned, SSE-S3 encrypted at rest (bucket
#     key), all public access blocked, ACLs disabled (BucketOwnerEnforced),
#     TLS-only bucket policy, noncurrent-version expiry, prevent_destroy.
#     Locking uses the S3-native lock file (use_lockfile = true in the
#     staging backend) — no DynamoDB table.
#   - optionally, a monthly account cost budget with email alerts.
#
# SSE-S3 (not a customer-managed KMS key): after ADR 0086 the staging state
# holds no secret values, and a customer-managed key would add a standing
# $1/month plus key-policy management to a bucket that must outlive every
# staging teardown. Access control is IAM (the deployer policy scopes the
# state object keys) plus the bucket policy below.

resource "aws_s3_bucket" "state" {
  bucket = var.state_bucket_name

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_ownership_controls" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket = aws_s3_bucket.state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    id     = "expire-superseded-state-versions"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.noncurrent_version_retention_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.state]
}

data "aws_caller_identity" "current" {}

data "aws_iam_policy_document" "state_bucket" {
  # Nobody (the deployer included) may delete state versions or the bucket
  # itself: old versions age out only via the lifecycle rule above, which
  # S3 applies itself and is not subject to this policy. An account admin
  # who genuinely needs to must first edit this policy — a deliberate,
  # CloudTrail-visible step.
  statement {
    sid     = "DenyStateVersionAndBucketDeletion"
    effect  = "Deny"
    actions = ["s3:DeleteObjectVersion", "s3:DeleteBucket"]
    resources = [
      aws_s3_bucket.state.arn,
      "${aws_s3_bucket.state.arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }
  }

  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.state.arn,
      "${aws_s3_bucket.state.arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "state" {
  bucket = aws_s3_bucket.state.id
  policy = data.aws_iam_policy_document.state_bucket.json

  depends_on = [aws_s3_bucket_public_access_block.state]
}

resource "aws_budgets_budget" "account_monthly" {
  count = var.budget_alert_email != null ? 1 : 0

  name         = "igaming-platform-account-monthly"
  budget_type  = "COST"
  limit_amount = tostring(var.monthly_budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  dynamic "notification" {
    for_each = [50, 80, 100]
    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = notification.value
      threshold_type             = "PERCENTAGE"
      notification_type          = "ACTUAL"
      subscriber_email_addresses = [var.budget_alert_email]
    }
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = [var.budget_alert_email]
  }
}

# --- Permissions boundary for every staging ECS role (see file header) ---
#
# Exactly what ECS execution roles need at task start — nothing else. Any
# role the deployer creates is capped at this even if someone attaches a
# broader inline policy to it.
data "aws_iam_policy_document" "staging_ecs_role_boundary" {
  statement {
    sid       = "EcrAuthToken"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid = "EcrPullStagingRepos"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
    ]
    resources = ["arn:aws:ecr:${var.aws_region}:${data.aws_caller_identity.current.account_id}:repository/igaming-staging/*"]
  }

  statement {
    sid       = "StagingLogStreams"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["arn:aws:logs:${var.aws_region}:${data.aws_caller_identity.current.account_id}:log-group:/ecs/igaming-staging/*"]
  }

  statement {
    sid       = "StagingAppSecrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = ["arn:aws:secretsmanager:${var.aws_region}:${data.aws_caller_identity.current.account_id}:secret:igaming-staging/*"]
  }

  # The RDS-managed master secret ("rds!db-<uuid>"), only for the staging DB.
  statement {
    sid       = "StagingRdsManagedMasterSecret"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = ["arn:aws:secretsmanager:${var.aws_region}:${data.aws_caller_identity.current.account_id}:secret:rds!*"]

    condition {
      test     = "StringLike"
      variable = "secretsmanager:ResourceTag/aws:rds:primaryDBInstanceArn"
      values   = ["arn:aws:rds:${var.aws_region}:${data.aws_caller_identity.current.account_id}:db:igaming-staging-*"]
    }
  }
}

resource "aws_iam_policy" "staging_ecs_role_boundary" {
  name        = "igaming-staging-ecs-role-boundary"
  description = "Permissions boundary for every igaming-staging ECS role (ADR 0086). Created by deploy/aws/bootstrap; the deployer cannot modify it."
  policy      = data.aws_iam_policy_document.staging_ecs_role_boundary.json
}
