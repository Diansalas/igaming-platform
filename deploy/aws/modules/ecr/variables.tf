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

variable "tags" {
  type    = map(string)
  default = {}
}
