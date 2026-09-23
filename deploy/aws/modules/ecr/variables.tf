variable "name_prefix" {
  type = string
}

variable "repository_names" {
  type    = list(string)
  default = ["platform-api", "b2c", "backoffice"]
}

variable "untagged_image_expiry_days" {
  description = "Lifecycle policy: expire untagged images after this many days."
  type        = number
  default     = 1
}

variable "force_delete" {
  description = "Allow terraform destroy to delete repositories that still contain images. Default false; the disposable staging root sets true (ADR 0086)."
  type        = bool
  default     = false
}

variable "tags" {
  type    = map(string)
  default = {}
}
