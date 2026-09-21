# ACM certificate request + DNS validation ONLY. This module is
# instantiated with `count` in the root module, strictly conditional on
# both `domain_name` and `route53_zone_id` being supplied — see
# environments/staging/main.tf. It deliberately does NOT create the
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
