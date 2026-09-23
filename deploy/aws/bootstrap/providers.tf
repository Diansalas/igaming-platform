terraform {
  required_version = ">= 1.11"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.100"
    }
  }

  # Bootstrap state is LOCAL and git-ignored on purpose: this root creates
  # the remote-state bucket itself, so it cannot store its own state there
  # before the bucket exists. It holds no secrets (bucket name/ARN, budget
  # name). If it is lost, nothing breaks: the bucket keeps working as the
  # staging backend, and the root can be re-adopted with
  #   terraform import aws_s3_bucket.state <bucket-name>
  # (plus the sibling bucket-configuration resources) — see
  # docs/runbooks/stage-9-4-staging-lifecycle-runbook.md §1.
}

provider "aws" {
  region              = var.aws_region
  allowed_account_ids = var.allowed_account_ids

  default_tags {
    tags = {
      Project     = "igaming-platform"
      Environment = "staging"
      ManagedBy   = "terraform"
      Component   = "bootstrap"
      StageRef    = "9.4"
    }
  }
}
