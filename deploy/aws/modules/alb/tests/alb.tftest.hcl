mock_provider "aws" {
  mock_resource "aws_lb" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:loadbalancer/app/igaming-staging-alb/0123456789abcdef"
    }
  }

  mock_resource "aws_lb_listener" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:listener/app/igaming-staging-alb/0123456789abcdef/0123456789abcdef"
    }
  }

  mock_resource "aws_lb_target_group" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:eu-central-1:765578795051:targetgroup/igaming-staging-tg/0123456789abcdef"
    }
  }
}

variables {
  name_prefix       = "igaming-staging"
  vpc_id            = "vpc-0aaaaaaaaaaaaaaa1"
  subnet_ids        = ["subnet-0aaaaaaaaaaaaaaa1", "subnet-0aaaaaaaaaaaaaaa2"]
  security_group_id = "sg-0aaaaaaaaaaaaaaa1"
}

run "internal_header_routed_alb" {
  command = apply

  variables {
    internal            = true
    routing_header_name = "X-Igaming-Service"
  }

  assert {
    condition     = aws_lb.this.internal == true
    error_message = "Staging ALB must be internal."
  }

  assert {
    condition     = one(aws_lb_listener.http.default_action).type == "fixed-response"
    error_message = "Unrouted requests must get a fixed 404, never a default forward."
  }

  assert {
    condition     = alltrue([for r in aws_lb_listener_rule.http : one(r.condition).http_header[0].http_header_name == "X-Igaming-Service"])
    error_message = "Rules must route on the header in header mode."
  }

  assert {
    condition     = length(aws_lb_listener_rule.http) == 3 && length(aws_lb_listener.https) == 0
    error_message = "Three HTTP rules and no HTTPS listener without a certificate."
  }
}

run "host_mode_requires_hostnames" {
  command = plan

  expect_failures = [aws_lb.this]
}
