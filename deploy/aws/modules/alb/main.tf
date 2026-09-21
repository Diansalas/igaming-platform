# Application Load Balancer with host-based routing for the three
# conceptual staging hostnames (api/app/admin). HTTPS + ACM + Route53 are
# handled entirely outside this module (see modules/dns and the root
# module) — this module only ever receives an optional certificate_arn,
# and is genuinely conditional on it: when null, it falls back to plain
# HTTP on the ALB's own DNS name, per the explicit instruction not to
# fabricate a domain decision.
#
# Host-based routing works even in the no-domain fallback: a caller can
# still exercise it by sending `curl -H "Host: <hostname>" http://<alb-dns-name>/...`
# — the ALB routes on the Host header regardless of whether that hostname
# actually resolves in DNS. This is documented in the runbook as a
# smoke-test-only technique for the fallback case, not a substitute for a
# real domain.

locals {
  https_enabled = var.certificate_arn != null
}

resource "aws_lb" "this" {
  name               = "${var.name_prefix}-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [var.security_group_id]
  subnets            = var.public_subnet_ids

  tags = merge(var.tags, { Name = "${var.name_prefix}-alb" })
}

resource "aws_lb_target_group" "platform_api" {
  name        = "${var.name_prefix}-api-tg"
  port        = var.container_port
  protocol    = "HTTP"
  vpc_id      = var.vpc_id
  target_type = "ip"

  health_check {
    # Readiness, not liveness — per docs/architecture/38-deployment-architecture.md:
    # the ALB must only route to instances passing /readyz (DB reachable).
    path                = "/readyz"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-api-tg" })
}

resource "aws_lb_target_group" "b2c" {
  name        = "${var.name_prefix}-b2c-tg"
  port        = var.container_port
  protocol    = "HTTP"
  vpc_id      = var.vpc_id
  target_type = "ip"

  health_check {
    path                = "/"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-b2c-tg" })
}

resource "aws_lb_target_group" "backoffice" {
  name        = "${var.name_prefix}-backoffice-tg"
  port        = var.container_port
  protocol    = "HTTP"
  vpc_id      = var.vpc_id
  target_type = "ip"

  health_check {
    path                = "/"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-backoffice-tg" })
}

# --- HTTP listener (always created) ---
#
# When HTTPS is enabled, ALL HTTP traffic is redirected to HTTPS
# (regardless of host/path) rather than duplicating host-based rules on
# both listeners. When HTTPS is not enabled (no domain supplied), the
# default action forwards to platform-api (useful for hitting /healthz
# and /readyz directly against the raw ALB DNS name), and explicit
# host-based rules handle the b2c/backoffice hostnames via a Host header
# override.
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.this.arn
  port              = 80
  protocol          = "HTTP"

  dynamic "default_action" {
    for_each = local.https_enabled ? [1] : []
    content {
      type = "redirect"
      redirect {
        port        = "443"
        protocol    = "HTTPS"
        status_code = "HTTP_301"
      }
    }
  }

  dynamic "default_action" {
    for_each = local.https_enabled ? [] : [1]
    content {
      type             = "forward"
      target_group_arn = aws_lb_target_group.platform_api.arn
    }
  }
}

resource "aws_lb_listener_rule" "http_api" {
  count        = local.https_enabled ? 0 : 1
  listener_arn = aws_lb_listener.http.arn
  priority     = 10

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.platform_api.arn
  }

  condition {
    host_header {
      values = [var.api_hostname]
    }
  }
}

resource "aws_lb_listener_rule" "http_app" {
  count        = local.https_enabled ? 0 : 1
  listener_arn = aws_lb_listener.http.arn
  priority     = 20

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.b2c.arn
  }

  condition {
    host_header {
      values = [var.app_hostname]
    }
  }
}

resource "aws_lb_listener_rule" "http_admin" {
  count        = local.https_enabled ? 0 : 1
  listener_arn = aws_lb_listener.http.arn
  priority     = 30

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.backoffice.arn
  }

  condition {
    host_header {
      values = [var.admin_hostname]
    }
  }
}

# --- HTTPS listener (genuinely conditional on certificate_arn) ---

resource "aws_lb_listener" "https" {
  count             = local.https_enabled ? 1 : 0
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.platform_api.arn
  }
}

resource "aws_lb_listener_rule" "https_app" {
  count        = local.https_enabled ? 1 : 0
  listener_arn = aws_lb_listener.https[0].arn
  priority     = 20

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.b2c.arn
  }

  condition {
    host_header {
      values = [var.app_hostname]
    }
  }
}

resource "aws_lb_listener_rule" "https_admin" {
  count        = local.https_enabled ? 1 : 0
  listener_arn = aws_lb_listener.https[0].arn
  priority     = 30

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.backoffice.arn
  }

  condition {
    host_header {
      values = [var.admin_hostname]
    }
  }
}

resource "aws_lb_listener_rule" "https_api" {
  count        = local.https_enabled ? 1 : 0
  listener_arn = aws_lb_listener.https[0].arn
  priority     = 10

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.platform_api.arn
  }

  condition {
    host_header {
      values = [var.api_hostname]
    }
  }
}
