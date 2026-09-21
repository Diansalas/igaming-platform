variable "name_prefix" {
  type = string
}

variable "db_master_username" {
  type    = string
  default = "igaming"
}

variable "db_runtime_username" {
  type    = string
  default = "igaming_runtime"
}

variable "tags" {
  type    = map(string)
  default = {}
}
