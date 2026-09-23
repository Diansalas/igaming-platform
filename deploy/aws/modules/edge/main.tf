# Staging edge (ADR 0086): browser-trusted HTTPS + viewer access control in
# front of an INTERNAL ALB, without a custom domain.
#
#   viewer --HTTPS--> CloudFront (*.cloudfront.net default certificate)
#          --VPC origin (AWS-private path, never the public internet)-->
#          internal ALB (private subnets, no public IPs) --> ECS tasks
#
# - TLS: each distribution serves HTTPS on its own *.cloudfront.net name
#   with CloudFront's default certificate, so no domain, no ACM certificate
#   and no Route53 zone are required. The API distribution is https-only
#   (plain-HTTP requests are rejected, never redirected, so an API client
#   misconfigured to plain HTTP fails loudly); the two frontends redirect
#   HTTP to HTTPS. The managed SecurityHeadersPolicy adds HSTS so a
#   browser that has visited once never retries over plain HTTP.
# - Origin leg: CloudFront VPC origins reach the internal ALB through
#   AWS-managed elastic network interfaces inside this VPC, so the
#   CloudFront -> ALB hop stays on the AWS private network even though it
#   is plain HTTP. The ALB has no public address at all.
# - Access control: a CloudFront Function on viewer-request admits only
#   viewers whose IPv4 source address is inside var.allowed_viewer_cidrs
#   and returns 403 to everyone else before anything reaches the origin.
#   This is what keeps the staging application — including its
#   test-support/simulation endpoints — away from the general public
#   internet. It is IP allowlisting, not user authentication: the
#   application's own authentication and authorization are unchanged.
# - Routing: each distribution stamps origin custom header
#   var.routing_header_name with its own service key (platform-api / b2c /
#   backoffice); modules/alb routes on it. CloudFront overwrites any
#   same-named header a viewer sends, so a viewer cannot pick another
#   service's route through a distribution.
# - Caching is disabled everywhere (managed CachingDisabled policy): every
#   request goes to the origin, exactly as it would without CloudFront.
#   All viewer headers except Host (managed AllViewerExceptHostHeader) are
#   forwarded, including Authorization, Origin and cookies.

locals {
  distributions = {
    platform_api = {
      comment                = "platform-api"
      header_value           = "platform-api"
      viewer_protocol_policy = "https-only"
    }
    b2c = {
      comment                = "b2c frontend"
      header_value           = "b2c"
      viewer_protocol_policy = "redirect-to-https"
    }
    backoffice = {
      comment                = "backoffice frontend"
      header_value           = "backoffice"
      viewer_protocol_policy = "redirect-to-https"
    }
  }
}

data "aws_cloudfront_cache_policy" "caching_disabled" {
  name = "Managed-CachingDisabled"
}

data "aws_cloudfront_origin_request_policy" "all_viewer_except_host" {
  name = "Managed-AllViewerExceptHostHeader"
}

data "aws_cloudfront_response_headers_policy" "security_headers" {
  name = "Managed-SecurityHeadersPolicy"
}

resource "aws_cloudfront_function" "viewer_ip_allowlist" {
  name    = "${var.name_prefix}-viewer-ip-allowlist"
  runtime = "cloudfront-js-2.0"
  comment = "Staging viewer IPv4 allowlist (ADR 0086)"
  publish = true
  code = templatefile("${path.module}/viewer-ip-allowlist.js.tftpl", {
    allowed_cidrs_json = jsonencode(var.allowed_viewer_cidrs)
  })
}

resource "aws_cloudfront_vpc_origin" "alb" {
  vpc_origin_endpoint_config {
    name                   = "${var.name_prefix}-alb"
    arn                    = var.alb_arn
    http_port              = 80
    https_port             = 443
    origin_protocol_policy = "http-only"

    origin_ssl_protocols {
      items    = ["TLSv1.2"]
      quantity = 1
    }
  }

  # VPC origin create/delete each take several minutes.
  timeouts {
    create = "30m"
    delete = "30m"
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-alb-vpc-origin" })
}

resource "aws_cloudfront_distribution" "this" {
  for_each = local.distributions

  enabled         = true
  comment         = "${var.name_prefix} ${each.value.comment}"
  is_ipv6_enabled = false # the viewer allowlist is IPv4-only (see header)
  http_version    = "http2and3"
  price_class     = var.price_class

  origin {
    origin_id   = "internal-alb"
    domain_name = var.alb_dns_name

    vpc_origin_config {
      vpc_origin_id = aws_cloudfront_vpc_origin.alb.id
    }

    custom_header {
      name  = var.routing_header_name
      value = each.value.header_value
    }
  }

  default_cache_behavior {
    target_origin_id       = "internal-alb"
    viewer_protocol_policy = each.value.viewer_protocol_policy
    allowed_methods        = ["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"]
    cached_methods         = ["GET", "HEAD"]
    compress               = true

    cache_policy_id            = data.aws_cloudfront_cache_policy.caching_disabled.id
    origin_request_policy_id   = data.aws_cloudfront_origin_request_policy.all_viewer_except_host.id
    response_headers_policy_id = data.aws_cloudfront_response_headers_policy.security_headers.id

    function_association {
      event_type   = "viewer-request"
      function_arn = aws_cloudfront_function.viewer_ip_allowlist.arn
    }
  }

  restrictions {
    geo_restriction {
      restriction_type = "none"
    }
  }

  viewer_certificate {
    cloudfront_default_certificate = true
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-${each.key}-cdn" })
}
