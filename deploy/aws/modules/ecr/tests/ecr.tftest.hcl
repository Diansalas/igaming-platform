mock_provider "aws" {}

variables {
  name_prefix = "igaming-staging"
}

run "immutable_scanned_repositories" {
  command = plan

  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.image_tag_mutability == "IMMUTABLE"])
    error_message = "ECR tags must be immutable (ADR 0086)."
  }

  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.image_scanning_configuration[0].scan_on_push])
    error_message = "Scan-on-push must stay enabled."
  }

  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.force_delete == false])
    error_message = "force_delete must default to false outside disposable environments."
  }
}
