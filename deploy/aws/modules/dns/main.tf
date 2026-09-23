# ACM certificate request + DNS validation ONLY.
#
# NOT WIRED INTO ANY ENVIRONMENT since Stage 9.4 (ADR 0086 §12): staging now
# terminates TLS at CloudFront on its *.cloudfront.net name and needs no
# domain. Kept for a future custom-domain path (a public ALB with its own
# certificate — note that a CloudFront custom domain would instead need an
# ACM certificate in us-east-1). Validated offline by
# deploy/aws/tests/run-static-checks.sh. When it was wired (Stage 9.3) it was
# instantiated with `count`, strictly conditional on both `domain_name` and
# `route53_zone_id` being supplied. It deliberately does NOT create the
# alias A records pointing at the ALB: those live in the root module
# instead, because they depend on the ALB's own dns_name/zone_id, and the
# ALB's HTTPS listener depends on this module's certificate_arn output —
# putting both directions inside a single module pair would create a
# circular module dependency. The root module breaks the cycle by only
# ever consuming outputs in one direction: dns -> alb -> root-level alias
# records.

resource "aws_acm_certificate" "this" {
  domain_name               = var.hostnames[0]
  subject_alternative_names = slice(var.hostnames, 1, length(var.hostnames))
  validation_method         = "DNS"

  lifecycle {
    create_before_destroy = true
  }

  tags = merge(var.tags, { Name = "igaming-staging-cert" })
}

resource "aws_route53_record" "validation" {
  for_each = {
    for dvo in aws_acm_certificate.this.domain_validation_options : dvo.domain_name => {
      name   = dvo.resource_record_name
      type   = dvo.resource_record_type
      record = dvo.resource_record_value
    }
  }

  zone_id         = var.route53_zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "this" {
  certificate_arn         = aws_acm_certificate.this.arn
  validation_record_fqdns = [for r in aws_route53_record.validation : r.fqdn]
}
