output "certificate_arn" {
  # Depending on the *_validation resource (not the raw certificate)
  # ensures anything consuming this output waits for DNS validation to
  # actually complete before, e.g., an ALB HTTPS listener tries to use it.
  value = aws_acm_certificate_validation.this.certificate_arn
}
