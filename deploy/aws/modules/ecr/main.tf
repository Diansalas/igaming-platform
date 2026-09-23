# One ECR repository per deployable image: platform-api (the Go binary,
# also used for the one-off migration/role-init tasks — same image,
# different container command override, see deploy/docker/platform-api.Dockerfile),
# b2c, backoffice (static SPA builds served by nginx).
#
# IMAGE IDENTITY (ADR 0086): tags are IMMUTABLE. A deployed tag is the full
# git commit SHA the image was built from (deploy/aws/scripts/deploy.sh), so
# a given tag can never be silently re-pointed at different image content —
# the mutable "latest" tag that Stage 9.3 defaulted to is no longer used as
# a deployment identity at all. Scan-on-push stays enabled.
#
# force_delete (default false) lets `terraform destroy` delete a repository
# that still contains images. Only a disposable environment should set it:
# the staging root does, because its images are rebuilt from git on every
# create and must not survive (and keep billing storage after) a teardown.

resource "aws_ecr_repository" "this" {
  for_each = toset(var.repository_names)

  name                 = "${var.name_prefix}/${each.key}"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = var.force_delete

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-${each.key}-ecr" })
}

resource "aws_ecr_lifecycle_policy" "untagged" {
  for_each   = aws_ecr_repository.this
  repository = each.value.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after ${var.untagged_image_expiry_days} day(s)"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = var.untagged_image_expiry_days
        }
        action = {
          type = "expire"
        }
      }
    ]
  })
}
