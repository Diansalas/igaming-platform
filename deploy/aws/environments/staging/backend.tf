# No remote backend is configured by default — local state is acceptable
# for a first staging deployment and keeps `terraform init -backend=false`
# (used for offline validation, see the runbook) working with zero setup.
#
# Before any real `terraform apply`, an operator should switch to a remote
# backend (S3 + DynamoDB lock table, or Terraform Cloud) so state isn't
# only on one laptop. Example (uncomment and fill in real values that
# exist in the confirmed AWS account before using):
#
# terraform {
#   backend "s3" {
#     bucket         = "igaming-platform-staging-tfstate"
#     key            = "stage-9-3/staging/terraform.tfstate"
#     region         = "eu-west-1"
#     dynamodb_table = "igaming-platform-tfstate-lock"
#     encrypt        = true
#   }
# }
