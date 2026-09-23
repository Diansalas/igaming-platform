# Application Load Balancer routing to the three services (platform-api,
# b2c, backoffice). Two routing modes:
#
# - HOST routing (routing_header_name = null, the module default): listener
#   rules match the Host header against api/app/admin hostnames. This is the
#   production-shaped mode for a public ALB with its own domain; HTTPS is
#   genuinely conditional on certificate_arn (see modules/dns). With no
#   certificate the ALB serves plain HTTP and the default action forwards to
#   platform-api.
#
# - HEADER routing (routing_header_name set): listener rules match a request
#   header whose value names the service. This is what the staging root uses
#   (ADR 0086): the ALB is INTERNAL (var.internal = true, private subnets, no
#   public IPs) and is reached only through CloudFront VPC origins; each of
#   the three CloudFront distributions stamps its own origin custom header
#   (modules/edge), because CloudFront replaces the viewer's Host header with
#   the ALB's own DNS name. The header is a routing key, not a secret — an
#   internal ALB is not reachable from outside the VPC in the first place.
#   On the HTTP listener, any request without a matching header gets a
#   fixed 404, never a default forward to a service. (If a certificate is
#   ALSO supplied, the HTTPS listener keeps the host-mode default of
#   forwarding to platform-api; the staging root supplies no certificate.)

locals {
  https_enabled  = var.certificate_arn != null
  header_routing = var.routing_header_name != null

  # key => routing attributes. Priorities match the Stage 9.3 rules.
  routes = {
    platform_api = { priority = 10, hostname = var.api_hostname, header_value = "platform-api" }
    b2c          = { priority = 20, hostname = var.app_hostname, header_value = "b2c" }
    backoffice   = { priority = 30, hostname = var.admin_hostname, header_value = "backoffice" }
  }

  target_groups = {
    platform_api = aws_lb_target_group.platform_api.arn
    b2c          = aws_lb_target_group.b2c.arn
    backoffice   = aws_lb_target_group.backoffice.arn
  }
}

resource "aws_lb" "this" {
  name               = "${var.name_prefix}-alb"
  internal           = var.internal
  load_balancer_type = "application"
  security_groups    = [var.security_group_id]
  subnets            = var.subnet_ids

  # Reject requests carrying malformed/ambiguous headers rather than
  # forwarding them (request-smuggling hardening).
  drop_invalid_header_fields = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-alb" })

  lifecycle {
    precondition {
      condition     = local.header_routing || (var.api_hostname != null && var.app_hostname != null && var.admin_hostname != null)
      error_message = "Host routing (routing_header_name = null) requires api_hostname, app_hostname and admin_hostname."
    }
  }
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
# - HTTPS enabled: ALL HTTP traffic is redirected to HTTPS.
# - Header routing: unmatched requests get a fixed 404.
# - Otherwise (host routing, no HTTPS): the default action forwards to
#   platform-api (handy for /healthz and /readyz against the raw ALB DNS
#   name), and explicit host rules handle b2c/backoffice.
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
    for_each = !local.https_enabled && local.header_routing ? [1] : []
    content {
      type = "fixed-response"
      fixed_response {
        content_type = "text/plain"
        message_body = "Not Found"
        status_code  = "404"
      }
    }
  }

  dynamic "default_action" {
    for_each = !local.https_enabled && !local.header_routing ? [1] : []
    content {
      type             = "forward"
      target_group_arn = aws_lb_target_group.platform_api.arn
    }
  }
}

resource "aws_lb_listener_rule" "http" {
  for_each = local.https_enabled ? {} : local.routes

  listener_arn = aws_lb_listener.http.arn
  priority     = each.value.priority

  action {
    type             = "forward"
    target_group_arn = local.target_groups[each.key]
  }

  dynamic "condition" {
    for_each = local.header_routing ? [] : [1]
    content {
      host_header {
        values = [each.value.hostname]
      }
    }
  }

  dynamic "condition" {
    for_each = local.header_routing ? [1] : []
    content {
      http_header {
        http_header_name = var.routing_header_name
        values           = [each.value.header_value]
      }
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

resource "aws_lb_listener_rule" "https" {
  for_each = local.https_enabled ? local.routes : {}

  listener_arn = aws_lb_listener.https[0].arn
  priority     = each.value.priority

  action {
    type             = "forward"
    target_group_arn = local.target_groups[each.key]
  }

  dynamic "condition" {
    for_each = local.header_routing ? [] : [1]
    content {
      host_header {
        values = [each.value.hostname]
      }
    }
  }

  dynamic "condition" {
    for_each = local.header_routing ? [1] : []
    content {
      http_header {
        http_header_name = var.routing_header_name
        values           = [each.value.header_value]
      }
    }
  }
}
