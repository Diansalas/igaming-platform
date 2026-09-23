# ADR 0084 — Stage 9.3 Staging AWS Architecture

Status: Accepted (devops implementation, scoped to STAGING only). Owner:
`devops`. **Amended by ADR 0086 (Stage 9.4)** — for the staging
environment, the following parts of this ADR are superseded: the single
NAT Gateway and private-subnet ECS tasks (now public-IP tasks, no NAT),
the internet-facing HTTP ALB and domain fallback (now an internal ALB
behind IP-allowlisted CloudFront HTTPS distributions), Terraform-generated
master/runtime/JWT secrets and the two `DATABASE_URL` secrets (now
RDS-managed master password, write-only secrets, password-free
`DATABASE_URL` + `PGPASSWORD`), Container Insights and ECS running-task
alarms (now ALB healthy-host alarms), the unconditional SNS topic, the
single execution role (now service + one-off roles), mutable image tags,
local state, `eu-west-1` defaults, and PostgreSQL 16.4. The module
structure, role separation, deployment ordering and the "Promoting to
production" list below still stand.

## Context

`docs/decisions/0009-hosting-hyperscale-cloud.md` (ADR 0009) already
decided to target a major hyperscale cloud provider, left the final
provider selection (AWS/GCP/Azure) and the gambling AUP/legal
confirmation open as human/legal tasks, and explicitly stated: "Until
that confirmation exists, only development/staging environments are
provisioned — no production, no real-money workload." Stage 9.3 needs a
concrete staging deployment to exercise the deployment contract described
in `docs/architecture/38-deployment-architecture.md` (env-var-only config,
`/healthz`/`/readyz`, the separate migration step, `TRUSTED_PROXY_COUNT`,
role separation) against something more realistic than a laptop.

**This ADR records a STAGING deployment only.** It does not select AWS as
the final production provider, does not close ADR 0009's open AUP/legal
confirmation, and does not authorize any production workload. ADR 0009
remains the umbrella decision; this ADR is a scoped, reversible
implementation choice under it — staging environments were already
explicitly permitted, and this is the first one actually provisioned as
code (not yet applied against a real account — see the runbook's
prerequisites).

## Decision

Provision a staging environment on AWS using Terraform, sized as a
production-shaped-but-staging-sized architecture, per the explicit
instruction: "smallest safe AWS architecture for staging that can later
be promoted toward production without rearchitecting the application."
Nothing here is applied against a real AWS account by this decision —
that remains a separate, later operator action (see "What this decision
does not do", below).

### Module layout

`deploy/aws/modules/{network,security,database,ecr,secrets,iam,ecs,alb,dns,observability}` —
one module per infrastructure concern, each with staging-sized defaults
exposed as variables so the same modules can be reused, unmodified, for a
future `deploy/aws/environments/production` root module with different
variable values (see "Promoting to production", below). `deploy/aws/environments/staging`
is the root module wiring them together.

- **network**: isolated VPC, 2 AZs, public subnets (ALB) + private
  subnets (ECS + RDS, no public IP/route), a single shared NAT Gateway
  (explicitly a staging cost tradeoff, not a production pattern).
- **security**: three security groups forming internet → ALB → ECS →
  RDS, each layer reachable only from the layer in front of it.
- **database**: RDS PostgreSQL 16, private, KMS-encrypted at rest,
  `rds.force_ssl` enforced, automated backups (staging: 7-day retention,
  variable), `multi_az=false` and `deletion_protection=false` by default
  (both variables, flippable for production).
- **ecr**: 3 repositories (platform-api, b2c, backoffice), image scanning
  on push, untagged-image lifecycle expiry.
- **secrets**: exactly 3 Terraform-generated (`random_password`) secrets —
  DB master credentials, the `igaming_runtime` app credential, and
  `JWT_SIGNING_SECRET` — stored in Secrets Manager. No secret is ever
  typed into a tfvars file; there is nothing to put in one. Two further
  Secrets Manager secrets (`database_url_runtime`, `database_url_migration`)
  are created at the root module level, holding full `DATABASE_URL`
  connection strings *derived* from the above (host/port/dbname are
  infrastructure outputs, not fresh secrets) — necessary because ECS can
  only inject a secret's existing value as a container env var, not
  compose one from multiple sources at task-start time.
- **iam**: an ECS execution role scoped to exactly the ECR repos, secret
  ARNs, and CloudWatch log group ARNs this deployment creates (no wildcard
  resource, except the one AWS-mandated exception:
  `ecr:GetAuthorizationToken`, which AWS does not support scoping to a
  resource ARN at all); an empty/minimal task role, since the application
  makes no other AWS API calls today.
- **ecs**: Fargate cluster (Container Insights enabled), task definitions
  and services for platform-api/b2c/backoffice (all private-subnet, no
  public IP, behind the ALB), plus two one-off task definitions
  (`migrate`, `role-init`) run via `aws ecs run-task`, never as services.
- **alb**: host-based routing for three conceptual staging hostnames
  (api/app/admin). HTTPS + ACM + Route53 are genuinely conditional on
  `domain_name`+`route53_zone_id` both being supplied; when absent, the
  ALB serves plain HTTP on its own `*.elb.amazonaws.com` name — a
  documented, temporary, non-HTTPS fallback, not a fabricated domain
  decision.
- **dns**: ACM certificate request + DNS validation only (instantiated
  with `count` at the root, conditional on the same two variables). Route53
  alias A records live in the root module itself rather than inside this
  module, to avoid a circular module dependency between `dns` (needs to
  feed a cert ARN to `alb`) and `alb` (whose `dns_name`/`zone_id` the alias
  records need) — see `modules/dns/main.tf`'s header comment.
- **observability**: CloudWatch log groups (one per ECS service, 14-day
  staging retention), and 5 real alarms (ALB 5xx rate, per-target-group
  unhealthy-host count, RDS CPU, RDS free storage, ECS running-task-count
  below desired per service) wired to an SNS topic with an optional email
  subscription.

### Role separation on RDS

Per `docs/security/runtime-role-separation.md` and
`deploy/init-app-role.sql`'s dev/CI pattern: the RDS master username is
deliberately set to `igaming` (`deploy/aws/modules/database`'s
`master_username` variable) rather than a separate bootstrap identity,
because an RDS master user already owns the database/schema it creates
and holds `CREATEROLE` via the `rds_superuser` pseudo-role — it does not
need a further ownership handoff the way the dev Docker Postgres bootstrap
superuser does. A new one-off ECS task ("role-init") runs
`deploy/aws/sql/init-runtime-role.rds.sql` (an RDS-specific adaptation of
`deploy/init-app-role.sql`) connected as `igaming`, creating the
non-owning `igaming_runtime` role with exactly the runtime-role-separation
document's §3 privileges. `cmd/migrate` continues to run as `igaming`.

**Ordering correction applied during implementation:** the authoritative
narrowing of `igaming_runtime`'s access to `schema_migrations` (revoking
`INSERT`/`UPDATE`/`DELETE`) cannot be folded into the role-init step,
because `schema_migrations` does not exist until `cmd/migrate up` creates
it — exactly mirroring `.github/workflows/ci.yml`'s own three-step
sequence (create roles/grants → migrate → revoke). The deployment
therefore runs, in order: (1) role-init, (2) migrate (whose own ECS task
command chains the revoke immediately after `cmd/migrate up`, using the
same master credential, in the same task invocation, so the step can never
be silently skipped), (3) force a new deployment of the 3 services. This
is documented in `deploy/aws/modules/ecs`'s header comment and the
runbook.

## What this decision does not do

- It does not select AWS as the final production hosting provider. ADR
  0009's provider choice (AWS vs GCP vs Azure) and its gambling AUP/legal
  confirmation remain open, human/legal tasks.
- It does not provision anything against a real AWS account. This is
  Terraform code plus a runbook; `terraform init -backend=false`,
  `terraform fmt -check`, and `terraform validate` were run (all clean),
  but `terraform plan`/`apply` were deliberately never run — no AWS
  credentials were used or referenced, per the explicit instruction not to
  use this session's ambient, unconfirmed AWS credentials.
- It does not authorize any production or real-money workload. Every
  default in this package (deletion_protection=false, skip_final_snapshot,
  single NAT gateway, no WAF, short log/backup retention) is a staging
  choice and is called out as such everywhere it appears.

## Promoting to production (named explicitly, not left implicit)

When ADR 0009's provider/AUP confirmation is closed and a real production
deployment is authorized, promoting this same module set requires
changing (not rewriting):

1. `db_multi_az = true`, `db_deletion_protection = true`, a larger
   `db_instance_class`, `skip_final_snapshot = false` +
   `final_snapshot_identifier` set.
2. One NAT Gateway per AZ (the network module's single shared NAT is a
   named staging tradeoff/single point of failure).
3. Bigger ECS task CPU/memory and `desired_count` per service; a
   deliberate replica count decision informed by the rate-limiter's
   `limit × replicas` note in `docs/architecture/38-deployment-architecture.md`.
4. A WAF in front of the ALB (not present in this staging package at
   all).
5. A real domain + Route53 zone (not the non-HTTPS ALB-DNS-name
   fallback) — genuinely required for production, not merely
   recommended.
6. Longer CloudWatch log retention and backup retention windows.
7. Alarms wired to a real on-call channel (SNS → PagerDuty/Opsgenie/etc.),
   not just an optional single email address.
8. `APP_ENV=production` set *only* in that environment (never staging —
   see `docs/architecture/38-deployment-architecture.md` §2 point 6),
   which activates `db.VerifyRuntimeRoleInProduction`'s fail-closed check.
9. ADR 0009's still-open gambling AUP/legal confirmation must be closed,
   in writing, before any production workload — this is a human/legal
   task, not something this or any devops session can close by writing
   more Terraform.

## Related documents

`docs/decisions/0009-hosting-hyperscale-cloud.md` (umbrella decision, still
open on provider/AUP), `docs/architecture/38-deployment-architecture.md`
(the deployment contract this package implements),
`docs/security/runtime-role-separation.md` (PLAT-ROLESPLIT-1, the role
split this package replicates on RDS),
`docs/runbooks/production-configuration-checklist.md` (every env var, incl.
`CORS_ALLOWED_ORIGINS`), `docs/runbooks/stage-9-3-staging-deployment-runbook.md`
(the operator procedure).
