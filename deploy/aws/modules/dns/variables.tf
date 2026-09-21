variable "route53_zone_id" {
  description = "Hosted zone ID the certificate's DNS validation records and the eventual alias A records are written into."
  type        = string
}

variable "hostnames" {
  description = "Fully-qualified hostnames the certificate must cover: [api, app, admin]."
  type        = list(string)

  validation {
    condition     = length(var.hostnames) == 3
    error_message = "Expected exactly 3 hostnames: api, app, admin."
  }
}

variable "tags" {
  type    = map(string)
  default = {}
}
