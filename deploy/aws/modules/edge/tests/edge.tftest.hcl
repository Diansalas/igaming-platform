# Pins the ADR 0086 staging edge: HTTPS-only API, IPv4-only viewer
# allowlist rendered into the CloudFront Function, per-service routing
# header, and the allowlist input validation.

mock_provider "aws" {
  mock_resource "aws_cloudfront_function" {
    defaults = {
      arn = "arn:aws:cloudfront::765578795051:function/igaming-staging-viewer-ip-allowlist"
    }
  }
}

variables {
  name_prefix          = "igaming-staging"
  alb_arn              = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:loadbalancer/app/igaming-staging-alb/0123456789abcdef"
  alb_dns_name         = "internal-igaming-staging-alb-123456789.eu-central-1.elb.amazonaws.com"
  routing_header_name  = "X-Igaming-Service"
  allowed_viewer_cidrs = ["203.0.113.10/32", "198.51.100.0/24"]
}

run "https_and_allowlist" {
  command = plan

  assert {
    condition     = aws_cloudfront_distribution.this["platform_api"].default_cache_behavior[0].viewer_protocol_policy == "https-only"
    error_message = "The API distribution must refuse plain HTTP outright."
  }

  assert {
    condition     = alltrue([for k in ["b2c", "backoffice"] : aws_cloudfront_distribution.this[k].default_cache_behavior[0].viewer_protocol_policy == "redirect-to-https"])
    error_message = "Frontends must redirect HTTP to HTTPS."
  }

  assert {
    condition     = alltrue([for d in aws_cloudfront_distribution.this : d.is_ipv6_enabled == false])
    error_message = "Distributions must be IPv4-only (the allowlist is IPv4-only)."
  }

  assert {
    condition     = strcontains(aws_cloudfront_function.viewer_ip_allowlist.code, "var ALLOWED_CIDRS = [\"203.0.113.10/32\",\"198.51.100.0/24\"];")
    error_message = "The allowlist must be rendered into the viewer-request function."
  }

  assert {
    condition = alltrue([
      for d in aws_cloudfront_distribution.this :
      length(d.default_cache_behavior[0].function_association) == 1 && one(d.default_cache_behavior[0].function_association).event_type == "viewer-request"
    ])
    error_message = "Every distribution must run the allowlist on viewer-request."
  }

  assert {
    condition = {
      for k, d in aws_cloudfront_distribution.this : k => one([for h in one(d.origin).custom_header : h.value if h.name == "X-Igaming-Service"])
      } == {
      platform_api = "platform-api"
      b2c          = "b2c"
      backoffice   = "backoffice"
    }
    error_message = "Each distribution must stamp its own routing header value."
  }
}

run "whole_internet_rejected" {
  command = plan

  variables {
    allowed_viewer_cidrs = ["0.0.0.0/0"]
  }

  expect_failures = [var.allowed_viewer_cidrs]
}

run "empty_allowlist_rejected" {
  command = plan

  variables {
    allowed_viewer_cidrs = []
  }

  expect_failures = [var.allowed_viewer_cidrs]
}

run "carrier_sized_block_rejected" {
  command = plan

  variables {
    allowed_viewer_cidrs = ["100.64.0.0/16"]
  }

  expect_failures = [var.allowed_viewer_cidrs]
}

run "missing_prefix_rejected_cleanly" {
  command = plan

  variables {
    allowed_viewer_cidrs = ["203.0.113.10"]
  }

  expect_failures = [var.allowed_viewer_cidrs]
}
