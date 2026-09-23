# Offline regression tests for the staging root (ADR 0086). Mock providers
# only: no AWS credentials, no network, no real resources. Run with
#   terraform init -backend=false && terraform test
# (see deploy/aws/tests/run-static-checks.sh, also run in CI).

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "765578795051"
    }
  }

  mock_data "aws_availability_zones" {
    defaults = {
      names = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  mock_resource "aws_db_instance" {
    defaults = {
      address = "igaming-staging-db.abc123.eu-central-1.rds.amazonaws.com"
      port    = 5432
      master_user_secret = [{
        secret_arn    = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:rds!db-mock-AbCdEf"
        kms_key_id    = "alias/aws/secretsmanager"
        secret_status = "active"
      }]
    }
  }

  mock_resource "aws_cloudfront_distribution" {
    defaults = {
      domain_name = "d1234example.cloudfront.net"
    }
  }

  # Realistic ARNs: the mock provider still runs the real provider's
  # argument validation, which rejects random strings where ARNs go.
  mock_resource "aws_lb" {
    defaults = {
      arn        = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:loadbalancer/app/igaming-staging-alb/0123456789abcdef"
      arn_suffix = "app/igaming-staging-alb/0123456789abcdef"
      dns_name   = "internal-igaming-staging-alb-123456789.eu-central-1.elb.amazonaws.com"
    }
  }

  mock_resource "aws_lb_listener" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:listener/app/igaming-staging-alb/0123456789abcdef/0123456789abcdef"
    }
  }

  mock_resource "aws_lb_target_group" {
    defaults = {
      arn        = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:targetgroup/igaming-staging-tg/0123456789abcdef"
      arn_suffix = "targetgroup/igaming-staging-tg/0123456789abcdef"
    }
  }

  mock_resource "aws_secretsmanager_secret" {
    defaults = {
      arn = "arn:aws:secretsmanager:eu-central-1:765578795051:secret:igaming-staging/mock-AbCdEf"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::765578795051:role/igaming-staging-mock-role"
    }
  }

  mock_resource "aws_ecr_repository" {
    defaults = {
      arn            = "arn:aws:ecr:eu-central-1:765578795051:repository/igaming-staging/mock"
      repository_url = "765578795051.dkr.ecr.eu-central-1.amazonaws.com/igaming-staging/mock"
    }
  }

  mock_resource "aws_cloudfront_function" {
    defaults = {
      arn = "arn:aws:cloudfront::765578795051:function/igaming-staging-viewer-ip-allowlist"
    }
  }

  mock_resource "aws_sns_topic" {
    defaults = {
      arn = "arn:aws:sns:eu-central-1:765578795051:igaming-staging-alerts"
    }
  }

  mock_resource "aws_ecs_cluster" {
    defaults = {
      arn = "arn:aws:ecs:eu-central-1:765578795051:cluster/igaming-staging-cluster"
    }
  }

  mock_resource "aws_ecs_task_definition" {
    defaults = {
      arn = "arn:aws:ecs:eu-central-1:765578795051:task-definition/igaming-staging-mock:1"
    }
  }
}

# The real hashicorp/random provider is used (not mocked): it is purely
# local (no network, no cloud), and mock providers do not support the
# ephemeral random_password resources modules/secrets relies on.

variables {
  image_tag            = "0123456789abcdef0123456789abcdef01234567"
  staging_access_cidrs = ["203.0.113.10/32"]
}

run "staging_defaults_are_hardened" {
  command = apply

  # --- region / account ---
  assert {
    condition     = output.aws_region == "eu-central-1"
    error_message = "Canonical staging region must be eu-central-1."
  }

  # --- RDS: private, pinned engine, master password never in Terraform ---
  assert {
    condition     = module.database.master_user_secret_arn != null
    error_message = "RDS must manage its own master password (manage_master_user_password)."
  }

  # --- no NAT Gateway in the default (public-IP task) mode ---
  assert {
    condition     = module.network.nat_gateway_id == null
    error_message = "Staging default must not create a NAT Gateway."
  }

  # --- outputs are HTTPS CloudFront URLs, never plain HTTP ---
  assert {
    condition     = startswith(output.api_url, "https://") && startswith(output.staging_url, "https://") && startswith(output.backoffice_url, "https://")
    error_message = "All staging URLs must be HTTPS."
  }

  # --- one-off tasks run where the services run, with a public IP ---
  assert {
    condition     = output.ecs_assign_public_ip == "ENABLED"
    error_message = "Public-IP task mode must hand ENABLED to deploy.sh's run-task."
  }

  # --- no SNS resources without an alarm email ---
  assert {
    condition     = output.sns_alerts_topic_arn == null
    error_message = "No SNS topic may exist when alarm_email is unset."
  }

  # --- secret ARNs exposed, never values ---
  assert {
    condition     = length(output.secret_arns) == 3 && alltrue([for v in values(output.secret_arns) : startswith(v, "arn:aws:secretsmanager:")])
    error_message = "secret_arns must list exactly the three secret ARNs."
  }
}

run "image_tag_rejects_latest" {
  command = plan

  variables {
    image_tag = "latest"
  }

  expect_failures = [var.image_tag]
}

run "image_tag_rejects_short_sha" {
  command = plan

  variables {
    image_tag = "0123456"
  }

  expect_failures = [var.image_tag]
}

run "region_is_pinned" {
  command = plan

  variables {
    aws_region = "eu-west-1"
  }

  expect_failures = [var.aws_region]
}

run "access_cidrs_reject_whole_internet" {
  command = plan

  variables {
    staging_access_cidrs = ["0.0.0.0/0"]
  }

  expect_failures = [var.staging_access_cidrs]
}

run "access_cidrs_reject_ipv6_and_wide_prefixes" {
  command = plan

  variables {
    staging_access_cidrs = ["2001:db8::/32", "10.0.0.0/8"]
  }

  expect_failures = [var.staging_access_cidrs]
}

run "multi_replica_acceptance_count_is_allowed" {
  command = plan

  variables {
    platform_api_desired_count = 2
  }
}

run "desired_count_upper_bound" {
  command = plan

  variables {
    platform_api_desired_count = 5
  }

  expect_failures = [var.platform_api_desired_count]
}

run "alarm_email_creates_sns" {
  command = apply

  variables {
    alarm_email = "oncall@example.com"
  }

  assert {
    condition     = output.sns_alerts_topic_arn != null
    error_message = "Setting alarm_email must create the SNS topic."
  }
}
