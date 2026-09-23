output "domain_names" {
  description = "map(service key => *.cloudfront.net domain name)."
  value       = { for k, d in aws_cloudfront_distribution.this : k => d.domain_name }
}

output "urls" {
  description = "map(service key => https:// origin URL, no trailing slash)."
  value       = { for k, d in aws_cloudfront_distribution.this : k => "https://${d.domain_name}" }
}

output "distribution_ids" {
  value = { for k, d in aws_cloudfront_distribution.this : k => d.id }
}
