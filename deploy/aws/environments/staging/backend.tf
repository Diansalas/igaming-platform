# Remote, encrypted, locked Terraform state for STAGING ONLY (ADR 0086).
#
# - Bucket: created ONCE by deploy/aws/bootstrap (see
#   docs/runbooks/stage-9-4-staging-lifecycle-runbook.md §1). It is
#   dedicated to this project's staging state in account 765578795051,
#   versioned, encrypted at rest (SSE-S3), public access fully blocked,
#   TLS-only by bucket policy, and outlives every staging create/destroy
#   cycle — state never lives in a disposable Claude Code / laptop session.
# - Locking: S3-native lock file (use_lockfile = true, Terraform >= 1.11;
#   a "<key>.tflock" object written with a conditional PUT). This replaces
#   the DynamoDB lock table, whose use HashiCorp has deprecated for the S3
#   backend — no DynamoDB table exists or is needed.
# - encrypt = true additionally requests server-side encryption on every
#   state write.
# - allowed_account_ids makes `terraform init` refuse any other account.
#
# The state holds no secret VALUES (ADR 0086 §"Secrets and Terraform
# state"), but it is still sensitive infrastructure metadata: treat the
# bucket as restricted, never copy state into the repository, never commit
# *.tfstate (see .gitignore).
#
# Offline validation (no AWS access at all): `terraform init -backend=false`.
# Plan BEFORE the bucket exists (read-only verification only): see the
# runbook's "pre-bootstrap plan" procedure (a local-backend override file,
# never committed).
terraform {
  backend "s3" {
    bucket              = "igaming-platform-staging-tfstate-765578795051"
    key                 = "staging/terraform.tfstate"
    region              = "eu-central-1"
    encrypt             = true
    use_lockfile        = true
    allowed_account_ids = ["765578795051"]
  }
}
