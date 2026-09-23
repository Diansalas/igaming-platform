# ADR 0086 — Stage 9.4 Staging Infrastructure Hardening + Cost Optimization

Status: Accepted (human-approved decision set, Stage 9.4, 2026-09-23).
Owner: `devops` (orchestrator-implemented). Independent reviews by
`architect`, `security`, FinOps, `backend`, `qa` and `code-reviewer`, and
what each found, are recorded in `docs/progress.md` (Stage 9.4 staging
hardening entry); this ADR describes the design as amended by those
reviews. Amends ADR 0084 (Stage 9.3 staging architecture) for
the **staging environment only**. Nothing here was applied to AWS: the
only AWS access used was the human-authorized read-only credential
(`arn:aws:iam::765578795051:user/claude-staging-readonly`).

## Context

The first real `terraform plan` of the Stage 9.3 package (read-only, account
765578795051, eu-central-1) planned 77 resources at roughly $135–155/month
and surfaced these defects:

1. PostgreSQL `16.4` is not offered in eu-central-1 — `apply` would fail at
   the RDS step after VPC/NAT/etc. were already created.
2. State was local-only, in a disposable session, and would have contained
   the RDS master password, the runtime password, the JWT signing secret
   and two full `DATABASE_URL` connection strings in plaintext; `.gitignore`
   did not exclude Terraform state at all.
3. Secrets Manager's default 30-day recovery window made destroy → recreate
   fail on secret-name collisions.
4. Both RDS alarms used the `DbiResourceId` dimension; AWS/RDS publishes
   `CPUUtilization`/`FreeStorageSpace` under `DBInstanceIdentifier`, so the
   alarms could never fire (and, with `notBreaching`, stayed silently green).
5. Container Insights (billed custom metrics) was on only to feed three
   running-task alarms; an SNS topic with no subscriber existed.
6. The ALB was internet-facing, HTTP-only, open to `0.0.0.0/0` — carrying
   credentials/JWTs in plaintext and exposing the test-support endpoints
   to anyone.
7. Images deployed under the mutable `latest` tag by default.
8. A NAT Gateway, two platform-api replicas and three on-demand tasks ran
   24/7, although staging only needs to exist during acceptance sessions.

The human approved a consolidated hardening pass with explicit decisions
(eu-central-1 canonical, PostgreSQL 16.15, remote encrypted state, etc.) and
asked for the cheapest design that stays acceptably secure for a
**non-production, synthetic-data, mock-provider** environment.

## Decisions

### 1. Region — eu-central-1 is canonical for staging

`aws_region` defaults to and is **validated** to `eu-central-1` (a region
change is a recorded decision, not a variable override); the state bucket,
backend and bootstrap live there too. Every stale `eu-west-1` staging
default/example was removed. No production region is implied.

### 2. PostgreSQL 16.15 (staging pin)

Verified read-only on 2026-09-23: `16.15` is `available` in eu-central-1
(family `postgres16`) and `db.t4g.micro` is orderable for it (gp3; AZs
a/b/c). The staging root pins `db_engine_version = "16.15"`; the database
module now has **no** engine-version default (each environment must pin a
verified version) plus a format validation. This is not a production
version policy.

### 3. Remote state — S3 with native locking, bootstrapped once

- **Backend**: `backend "s3"` (`deploy/aws/environments/staging/backend.tf`),
  bucket `igaming-platform-staging-tfstate-765578795051`, key
  `staging/terraform.tfstate`, `encrypt = true`, **`use_lockfile = true`**,
  `allowed_account_ids = ["765578795051"]`.
- **Locking**: S3-native lock files (Terraform ≥ 1.11, a conditional-write
  `<key>.tflock` object). HashiCorp has deprecated DynamoDB-based locking for
  the S3 backend, so there is **no DynamoDB table** — simpler and one less
  billable resource.
- **Bucket** (`deploy/aws/bootstrap`, applied once by a future authorized
  credential): versioning; SSE-S3 (AES256, bucket key); all four public
  access blocks; `BucketOwnerEnforced` (ACLs off); bucket policy denying
  non-TLS access and denying `s3:DeleteObjectVersion`/`s3:DeleteBucket` to
  everyone (old versions expire only via the lifecycle rule); noncurrent
  versions expire after 90 days; incomplete multipart uploads aborted after
  7 days; `prevent_destroy`. The same one-time bootstrap also creates the
  IAM permissions boundary every staging ECS role must carry and an IAM
  Access Analyzer account analyzer (decision 18) — 9 resources in total,
  plus the optional cost budget. SSE-S3 rather
  than a customer-managed KMS key: after decision 4 the state holds no
  secret values, and a CMK would add a standing $1/month plus key-policy
  management to a bucket that must outlive every teardown.
- **Isolation / least privilege**: bucket dedicated to this project's
  staging state in one account; the deployer policy grants object access to
  exactly `staging/terraform.tfstate` and its `.tflock`. No production state
  exists anywhere.
- **Bootstrap state** is local and git-ignored by design (it cannot live in
  the bucket it creates) and holds no secrets; losing it is recoverable by
  `terraform import`.
- Terraform `required_version >= 1.11`; providers `aws ~> 5.100` (last 5.x
  line; nothing required 6.x) and `random ~> 3.7`; root lock files committed
  with hashes for linux/darwin × amd64/arm64.

### 4. Secrets and Terraform state

Marking values `sensitive` does **not** keep them out of state; it only
redacts CLI output. The design instead keeps the values out of Terraform
entirely where the platform allows it:

| Secret | Stage 9.3 | Now | In state? |
|---|---|---|---|
| RDS master password | `random_password` → `aws_db_instance.password` | `manage_master_user_password = true`: RDS generates it and stores it in an RDS-managed Secrets Manager secret | **No** — only the secret ARN |
| `igaming_runtime` password | `random_password` → `secret_string` | `ephemeral "random_password"` → `secret_string_wo` (write-only) | **No** — only `secret_string_wo_version` |
| `JWT_SIGNING_SECRET` | same as above | same as above | **No** |
| First `platform_admin` password (new) | — (no staging path existed) | `ephemeral "random_password"` → `secret_string_wo`, injected only into the one-off `seed-admin` task | **No** |
| Runtime `DATABASE_URL` | Terraform-composed secret containing the password | plain env var **without** a password; the password arrives separately as `PGPASSWORD` | **No** secret (host/user/db only) |
| Migration `DATABASE_URL` | same | same, master password as `PGPASSWORD` | **No** secret |

`PGPASSWORD` works because pgx (`internal/db`, via pgconn's standard `PG*`
environment fallback) and `psql` both use it when the connection string
carries no password — verified empirically against the repo's pinned pgx,
including RDS-style special characters. No Go code changed.

**Logs, too (security review P1)**: the Stage 9.3 role-init script
printed the runtime password to stdout — `SELECT set_config(...)` returns
the value it sets — which the `awslogs` driver would have shipped to
CloudWatch Logs on every run; a failing `CREATE/ALTER ROLE` would also
have echoed it in the error context. Fixed in
`deploy/aws/sql/init-runtime-role.rds.sql` (`\o /dev/null` around the
`set_config`, failures re-raised with the SQLSTATE only, the session value
cleared after use) and **verified against a real PostgreSQL 16**: the old
script printed the password once; the fixed script prints it zero times on
create, idempotent re-run and a forced permission failure, and zero times
in the server log. A static guard pins it.

**What remains in state**: secret ARNs/names, usernames, the RDS endpoint,
resource IDs/ARNs, security-group rules, the CloudFront allowlist CIDRs
(also the operator's IP address) — infrastructure metadata, not
credentials. The state bucket is still treated as sensitive (restricted
IAM, TLS-only, versioned, never copied into git). Rotation: bump
`secret_version` (write-only values are rewritten only when the version
changes), re-run role-init, redeploy; RDS rotates its own master secret.

A static guard (`deploy/aws/tests/run-static-checks.sh`) and `terraform
test` assertions fail if a non-ephemeral `random_password`, a
`secret_string`, or a Terraform-supplied DB `password` is reintroduced.

### 5. Destroy/recreate — staging force-deletes, production default untouched

`modules/secrets` gains `recovery_window_in_days` (default **30**, validated
`0` or `7–30`); only the staging root passes `0` (force delete, names
reusable at once). `modules/ecr` gains `force_delete` (default `false`;
staging `true`). `modules/database` gains `create_kms_key` (default `true`;
staging `false` → AWS-managed `aws/rds` key, still encrypted), so a
teardown never leaves a customer-managed key pending deletion. Every
production-shaped default is unchanged; staging opts in explicitly.

### 6. RDS alarms — `DBInstanceIdentifier`

Alarms now take `rds_instance_identifier` (validated to reject the
`db-XXXX` resource-ID shape) and use `DBInstanceIdentifier`. Pinned by
`modules/observability/tests/alarms.tftest.hcl` and a repository grep guard.

### 7. Container Insights — off for staging

`modules/ecs` gains `container_insights_enabled` (default `true`; staging
`false`). The three ContainerInsights running-task alarms are replaced by
per-target-group **ALB `HealthyHostCount < 1`** alarms (free AWS/ELB
metrics), which catch a stopped, crash-looping or not-ready service — the
signal acceptance actually needs. Application logs are unchanged
(`awslogs` → CloudWatch Logs, 14-day retention).

### 8. SNS — conditional

The SNS topic and email subscription exist only when `alarm_email` is set;
otherwise alarms exist with no actions (visible in the console/CLI).

### 9. Fargate Spot — frontends Spot, platform-api mixed

- **b2c, backoffice**: `FARGATE_SPOT` only. A Spot interruption briefly takes
  a static site down and ECS replaces the task; acceptable for staging.
- **platform-api**: **B — mixed**: `FARGATE` `base = 1, weight = 0` +
  `FARGATE_SPOT` `weight = 1`. The first (and normally only) replica is
  on-demand, so an acceptance run cannot be broken by a Spot reclaim of the
  only API task; any extra replica (the multi-replica test) is Spot. Pure
  Spot (A) would save only ~$0.01/hour but make acceptance flaky; pure
  on-demand (C) costs more for the second replica for no benefit.
- Module default stays on-demand `FARGATE` only (production-shaped). The
  one-off migrate/role-init tasks run on-demand (minutes each).

### 10. Desired counts — 1 normally, 2 on demand

Staging `platform_api_desired_count` defaults to **1** (validated 1–4);
`deploy.sh scale 2` raises it for the multi-replica acceptance test and
`deploy.sh scale 1` lowers it again. The module default (2) and the
multi-replica architecture are unchanged; Stage 9.4 Part 1 already made the
activation seam and mock-provider references multi-replica-safe.

### 11. Network — no NAT; public-IP tasks, private RDS and ALB

Approximate fixed cost of each egress design in eu-central-1 (Pricing API,
2026-09-23; 730 h/month):

| Option | Fixed $/h | $/day | $/month | Usage charges | Security | Complexity |
|---|---|---|---|---|---|---|
| **A** private ECS + 1 NAT GW (Stage 9.3) | 0.057 (NAT 0.052 + EIP 0.005) | 1.37 | 41.61 | +$0.052/GB processed (image pulls, logs) | tasks have no public IP | low |
| **B** public-subnet ECS, public IPv4, SG ingress only from ALB SG — **chosen** | 0.015 (3 × 0.005; 0.020 during the 2-replica test) | 0.36 | 10.95 | none | tasks have public IPs but the SG admits only the ALB SG; RDS/ALB stay private | low |
| **C** private ECS + interface endpoints (ecr.api, ecr.dkr, logs, secretsmanager) + S3 gateway | 0.096 (4 × 2 AZ × 0.012) | 2.30 | 70.08 | +$0.01/GB | no public IPs, no internet egress at all | medium |
| C′ same, endpoints in one AZ | 0.048 | 1.15 | 35.04 | +$0.01/GB + cross-AZ | as C, no AZ redundancy | medium |
| **D1** IPv6/egress-only IGW (free) | ~0 | ~0 | ~0 | — | — | not verified for Fargate pulls/logs/secrets over IPv6 in this account → **rejected** as unverified |
| **D2** one task running all three containers | saves ~0.019 | | | | | breaks per-service target groups/scaling → **rejected** (architecture change) |

The application needs **no** general outbound internet: all providers are
in-process mocks and OTel exports to stdout. Tasks only need ECR, Secrets
Manager and CloudWatch Logs. B is cheapest, and its exposure is bounded by
the ECS security group (ingress only from the ALB security group on 8080).
**RDS remains in private subnets, `publicly_accessible = false`, reachable
only from the ECS security group, with a private route table that has no
default route at all.** Implemented as `modules/network`
`enable_nat_gateway` (default `true`) + `modules/ecs` `assign_public_ip`
(default `false`); the staging root flips both with one switch,
`ecs_public_ip_mode` (default `true`). Setting it to `false` restores the
production-shaped NAT layout.

### 12. HTTPS and access control without a domain — CloudFront VPC origins

```
browser / acceptance suite (allowlisted IPv4 only)
  --HTTPS (*.cloudfront.net default cert)--> 3 CloudFront distributions
      viewer-request CloudFront Function: IPv4 allowlist, else 403
  --CloudFront VPC origin (AWS private network)--> INTERNAL ALB (private subnets)
      header routing (X-Igaming-Service: platform-api|b2c|backoffice), else 404
  --> ECS tasks (SG: ALB SG only) --> RDS (SG: ECS SG only)
```

- **TLS without a domain**: each distribution serves HTTPS with CloudFront's
  default `*.cloudfront.net` certificate. No domain, ACM certificate or
  Route53 zone is needed, and none was invented. The API distribution is
  `https-only`; the frontends `redirect-to-https`; the managed
  SecurityHeadersPolicy adds HSTS. The origin leg (CloudFront → ALB) runs
  over CloudFront VPC origins on the AWS private network and never crosses
  the public internet in plaintext. The ALB is **internal** (no public
  IPs; its security group admits only the VPC CIDR, where the VPC-origin
  ENIs live).
- **Access control**: a CloudFront Function (viewer-request) admits only
  viewers inside `staging_access_cidrs` (required, no default, IPv4 only,
  prefix ≥ /16, validated at the root and in the module); everyone else
  gets 403 before anything reaches the origin. This protects the whole
  application, **including the test-support/simulation endpoints**, from
  the general internet. This is network allowlisting, not authentication:
  the application's own authN/authZ and the `APP_ENV` fail-closed gates are
  unchanged.
- **Browser practicality**: the operator opens `staging_url` and
  `backoffice_url` directly. The frontends call the API cross-origin at
  `api_url`, and `CORS_ALLOWED_ORIGINS` is computed from the two frontend
  distributions. `TRUSTED_PROXY_COUNT` becomes **2** (CloudFront + ALB) so
  rate limiting keys on the real viewer IP.
- **Cost**: no hourly charge for CloudFront, VPC origins or the function
  ($0.012 per 10k HTTPS requests, about $0.085/GB transfer, $0.10 per 1M
  function invocations). The internal ALB drops the ALB's public IPv4
  charges.
- **`TRUSTED_PROXY_COUNT = 2` evidence**: AWS documents that for custom
  origins CloudFront adds `X-Forwarded-For` with the viewer's TCP source IP
  when absent and **appends** it when the viewer sent one ("Request and
  response behavior for custom origins", Amazon CloudFront Developer
  Guide); the ALB then appends its own peer (the VPC-origin ENI). The
  application therefore sees `<anything the client sent>, <viewer>, <edge>`
  and `trustedProxyClientIP` takes the 2nd entry from the right — the real
  viewer, which a client cannot forge. (One review questioned this; the
  primary AWS documentation was checked and it holds; the other reviews
  concurred.)
- **ALB ingress** is the two **private subnet CIDRs** only (where the
  VPC-origin ENIs are created), not the whole VPC: the public-IP ECS tasks
  cannot reach the ALB.
- **Allowlist width**: prefixes shorter than **/24** are rejected (a /16
  could be a whole carrier/CGNAT block, and the test-support endpoints sit
  behind this allowlist).
- **Rejected**: a self-signed certificate on a public ALB (browser warnings
  on three origins, a private key in state, public ALB IP charges); WAF IP
  sets (~$5/month per web ACL plus rules for the same effect as a free
  function); HTTP Basic auth at the edge (it collides with the API's
  `Authorization: Bearer` header and CORS preflight).
- **A real domain is not required.** The custom-domain path (ADR 0084's
  `dns` module: ALB HTTPS + ACM + Route53) is no longer wired into staging
  but is kept in the repository. A CloudFront custom domain would need an
  ACM certificate in **us-east-1** plus human input (domain + hosted zone).
  Recorded as future work, not built.

### 13. Immutable image identity

ECR `image_tag_mutability = "IMMUTABLE"` (module default); scan-on-push
kept. `image_tag` has **no default** and must be a full 40-character git
SHA (validated; `latest` and short SHAs rejected). `deploy.sh` refuses a
dirty working tree, tags every image with `git rev-parse HEAD`, skips
already-pushed tags, and builds `--platform linux/amd64` to match Fargate's
X86_64 default. The frontend images embed their environment's API URL, so
they also carry it as the image label `igaming.api_url`; an existing tag
is reused only if the label matches the current environment (otherwise
`deploy.sh` stops and asks for a complete `down`).

### 14–15. Ephemeral lifecycle and cost

Staging is an **ephemeral acceptance environment**: `deploy.sh up` →
`deploy.sh seed-admin <email>` → acceptance → `deploy.sh scale 2` /
multi-replica test / `scale 1` → human inspection → `deploy.sh down` →
`verify-teardown.sh` (read-only; exits non-zero if anything billable
remains). **One commit per environment lifetime** (architecture review
P1): `up` refuses to deploy a different commit onto a running
environment, because a full apply would put the new code in front of the
old schema before migrations run, breaking
`docs/architecture/38-deployment-architecture.md` §2.3 — `down` then `up`
instead, which ephemeral staging makes cheap. The cost model is in the runbook
(`docs/runbooks/stage-9-4-staging-lifecycle-runbook.md` §7): about
**$0.10/hour while running**, versus about $0.19–0.21/hour for the Stage 9.3
design. After teardown only the state bucket (< $0.01/month), the free
IAM boundary policy and the optional budget remain. Free Tier and credits were **not** assumed in any
decision (§8 of the runbook lists where they might reduce the bill).

### 16. Test-support endpoints

`APP_ENV=staging` and `TEST_SUPPORT_ENDPOINTS_ENABLED=true` remain (the
acceptance flows need them). Their only exposure path, CloudFront → internal
ALB, is IP-allowlisted (decision 12). Nothing else reaches them: the ALB
is internal, the tasks' SG admits only the ALB, and unrouted ALB requests
get 404. The production fail-closed gates (`internal/config`) are untouched.

### 17. Execution-role separation (security review follow-through)

The single ECS execution role is split into a **service** role
(platform-api/b2c/backoffice: runtime DB secret + JWT secret only, cannot
read the RDS master credential) and a **one-off** role (migrate/role-init:
master + runtime secrets only). Runtime-role separation
(`docs/security/runtime-role-separation.md`) is therefore also enforced at
the IAM layer, not only by task-definition wiring.

### 18. Deployment credential least privilege (security review P1/P2)

The deployer policies (`deploy/aws/iam/`) are written so a deployer
credential **cannot escalate to account admin** and cannot touch
non-staging resources:

- **IAM**: role actions only on the three exact staging role names;
  `CreateRole`/`PutRolePolicy`/`DeleteRolePolicy` only when the role
  carries the permissions boundary `igaming-staging-ecs-role-boundary`
  (created by the human-run bootstrap: ECR pull from `igaming-staging/*`,
  staging log streams, staging secrets and the staging RDS-managed
  secret, nothing else); explicit **Deny** on removing/replacing
  boundaries, editing any policy, `AttachRolePolicy`,
  `UpdateAssumeRolePolicy`, creating users or access keys; `PassRole`
  only to `ecs-tasks.amazonaws.com`. The ECS trust policy also requires
  `aws:SourceAccount` = this account.
- **ELB/RDS/logs/alarms/SNS**: mutations scoped by name ARN
  (`igaming-staging-*`); **EC2**: deletes/detaches/route replacement only
  on resources tagged `Project=igaming-platform, Environment=staging`, and
  `CreateTags` only as tag-on-create; `elasticloadbalancing:*` replaced by
  an explicit list.
- **Secrets**: the deployer's own policy grants `GetSecretValue` only on
  `igaming-staging/*` (needed by the AWS provider to refresh
  `aws_secretsmanager_secret_version`), not on the RDS-managed master
  secret, and the state object cannot be deleted. This is **not** a claim
  that a deployer can never reach the master credential — see the residual
  below.
- All four documents validate with IAM Access Analyzer (0 findings), and
  `deploy/aws/tests/simulate-deployer-policies.py` (read-only
  `SimulateCustomPolicy`) shows 25/25 expectations — every escalation path
  the review described is denied, every operation the lifecycle needs is
  allowed.
- **Residual (accepted for synthetic staging, recorded)**: the deployer
  can still create/modify staging-named resources in ways the Terraform
  would not (e.g. an internet-facing ALB named `igaming-staging-*`, or
  loosen a staging security group) — it is the staging operator. EC2
  create/authorize actions are region-scoped rather than tag-scoped
  because EC2 evaluates new sub-resources (e.g. security-group rules) that
  carry no tag yet; tightening further needs a real-credential test.
- **Residual — role trust (security re-review N-1)**: IAM has no condition
  key that restricts the trust-policy principals of `CreateRole`. A
  deployer may `DeleteRole` + `CreateRole` one of the three staging role
  names with a trust policy naming itself or another account; the role
  still carries the boundary (no admin), but its holder could then read
  the staging secrets, including the RDS-managed master secret (as could a
  deployer running an ad-hoc one-off task). Accepted for a synthetic-data
  staging operator; **detection**: the bootstrap creates an IAM Access
  Analyzer account analyzer, which raises a finding for any role trusted
  from outside the account. Review its findings after each session.

### Deferred (recorded, not built in this pass)

- **Audit-record client IP** (security review P2): `internal/httpserver`'s
  audit call sites use `RemoteAddr`, which behind CloudFront → ALB is the
  ALB's private IP; only the rate limiter uses `TRUSTED_PROXY_COUNT`.
  Application change for `backend`; a **production launch gate**, not a
  staging blocker.
- Join duplicate `X-Forwarded-For` header lines before counting hops
  (defence in depth, application code).
- CloudFront/ALB access logs and VPC flow logs (cost vs. forensics for
  who called the test-support endpoints); RDS `sslmode=verify-full` with
  the RDS CA bundle (production).
- FinOps recommendation: serve the two SPAs from S3 behind their existing
  distributions (removes 2 tasks, 2 public IPv4, 2 target groups, ~20% of
  running cost) — an architecture change needing its own decision.

## What this decision does not do

- It does not apply anything to AWS, create credentials, or authorize
  deployment. The bootstrap and every `terraform apply` need a separate,
  human-authorized deployment credential (policies:
  `deploy/aws/iam/*.json`, all validated with IAM Access Analyzer).
- It does not change any production assumption. Module defaults remain
  production-shaped (NAT on, private tasks, Container Insights on, on-demand
  capacity, 30-day secret recovery, CMK for RDS, no force-delete, two API
  replicas). ADR 0084's "Promoting to production" list still applies. This
  ADR adds: production must not copy staging's `ecs_public_ip_mode`,
  `recovery_window_in_days = 0`, `force_delete`, `create_kms_key = false`,
  Spot-only frontends or the edge allowlist as its access model.
- It does not select a production domain, provider, or region. ADR 0009's
  AUP/legal confirmation stays open.

## Related

ADR 0084 (Stage 9.3 staging architecture, amended here), ADR 0085 (APP_ENV
fail-closed), ADR 0009 (hosting), `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md`,
`docs/runbooks/stage-9-4-aws-account-verification.md`,
`docs/security/runtime-role-separation.md`.
