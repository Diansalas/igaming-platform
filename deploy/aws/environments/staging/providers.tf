terraform {
  # >= 1.11: S3 backend native state locking (use_lockfile, backend.tf),
  # ephemeral resources and write-only arguments (modules/secrets) — ADR 0086.
  required_version = ">= 1.11"

  required_providers {
    aws = {
      source = "hashicorp/aws"
      # >= 5.100: aws_cloudfront_vpc_origin, secret_string_wo and the other
      # write-only arguments used here. Kept on the 5.x major.
      version = "~> 5.100"
    }
    random = {
      source = "hashicorp/random"
      # ephemeral "random_password" (modules/secrets).
      version = "~> 3.7"
    }
  }
}

provider "aws" {
  region = var.aws_region

  # Hard guard: every plan/apply fails immediately if the configured
  # credentials belong to any other AWS account.
  allowed_account_ids = var.allowed_account_ids

  default_tags {
    tags = {
      Project     = "igaming-platform"
      Environment = "staging"
      ManagedBy   = "terraform"
      StageRef    = "9.4"
    }
  }
}
