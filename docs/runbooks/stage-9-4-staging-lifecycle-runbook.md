# Stage 9.4 — Ephemeral Staging Lifecycle Runbook

Owner: `devops`. Design: `docs/decisions/0086-stage-9-4-staging-hardening-and-cost-optimization.md`
(amends ADR 0084). Account verification: `docs/runbooks/stage-9-4-aws-account-verification.md`.
Scripts: `deploy/aws/scripts/deploy.sh`, `deploy/aws/scripts/verify-teardown.sh`.

**Status when written: nothing in this runbook has been run against AWS.**
The only AWS access so far is the read-only credential
`arn:aws:iam::765578795051:user/claude-staging-readonly`, used only for
identity checks, `terraform plan`, pricing lookups, IAM Access Analyzer
policy validation, and a read-only `verify-teardown.sh` run (it found no
staging resources). Every AWS-changing step below needs a separate,
human-authorized **deployment credential** (§1.1).

Staging is an **ephemeral acceptance environment**, not a standing one:

```
bootstrap (once) ─► up ─► acceptance ─► scale 2 / multi-replica test / scale 1 ─► human inspection ─► down ─► verify-teardown
                    ▲                                                                                        │
                    └──────────────────────────── next session ──────────────────────────────────────────────┘
```

## 0. Prerequisites and human inputs

| Input | Where it goes | Notes |
|---|---|---|
| Deployment credential for account 765578795051 | operator shell (`aws configure` / SSO) | **Not yet requested or authorized.** Policies: §1.1. Never root; never the read-only user. |
| Your public IPv4 address(es) | `staging_access_cidrs` in `deploy/aws/environments/staging/terraform.tfvars` (git-ignored) | `curl -4 https://checkip.amazonaws.com` → `["x.x.x.x/32"]`. Also add wherever the acceptance suite runs from. Prefix ≥ /16; IPv4 only. |
| (Optional) alarm email | `alarm_email` in the same tfvars | Creates an SNS topic + subscription (confirm the email AWS sends). |
| (Optional) budget email | `budget_alert_email` for `deploy/aws/bootstrap` | Account-wide monthly cost budget alerts (backstop for "left running"). |

Tools: Terraform ≥ 1.11 (tested with 1.16.3), AWS CLI v2, Docker with
buildx (`--platform linux/amd64`), `jq`, `git`, `curl`.

## 1. One-time bootstrap (not part of each session)

### 1.1 Deployment credential (human action)

Create an IAM principal in 765578795051 for deployments (preferably a
role assumed via SSO, or an IAM user with MFA and short-lived keys) and
attach these customer-managed policies from the repository (all three
validated with IAM Access Analyzer `ValidatePolicy`: 0 findings):

- `deploy/aws/iam/staging-bootstrap-policy.json` — state bucket + budget
  (only needed for §1.2; detach afterwards).
- `deploy/aws/iam/staging-deployer-infra-policy.json` — VPC/EC2 networking,
  RDS, ECS, ELB, CloudWatch Logs/alarms, optional SNS, all conditioned on
  `aws:RequestedRegion = eu-central-1`.
- `deploy/aws/iam/staging-deployer-edge-iam-state-policy.json` — state
  object + lock file only, Secrets Manager (`igaming-staging/*` and RDS's
  `rds!*`), KMS only via RDS/Secrets Manager, ECR `igaming-staging/*` (incl.
  push), CloudFront (distributions, VPC origins, functions), IAM roles
  `igaming-staging-*` only, `iam:PassRole` only to `ecs-tasks.amazonaws.com`,
  service-linked roles only for ECS/ELB/RDS/CloudFront VPC origin, and the
  read-only calls `verify-teardown.sh` makes.

Then re-run the §1 identity check of the verification runbook against the
new credential and IAM's policy simulator (`aws iam simulate-principal-policy`)
for the actions above.

### 1.2 State bucket (+ optional budget)

```bash
cd deploy/aws/bootstrap
terraform init
terraform plan  -var 'budget_alert_email=you@example.com'   # omit the var for no budget
terraform apply -var 'budget_alert_email=you@example.com'   # review, then type yes
```

Creates `igaming-platform-staging-tfstate-765578795051` (eu-central-1,
versioned, SSE-S3, public access blocked, TLS-only, `prevent_destroy`) and,
if requested, the `igaming-platform-account-monthly` budget. Bootstrap state
stays local in `deploy/aws/bootstrap/terraform.tfstate` (git-ignored, no
secrets). Keep a copy somewhere safe. If it is lost, the bucket keeps
working; re-adopt it with `terraform import aws_s3_bucket.state
igaming-platform-staging-tfstate-765578795051` (and the sibling
`aws_s3_bucket_*` resources) before changing bootstrap again.

### 1.3 Initialise the staging backend

```bash
cd deploy/aws/environments/staging
terraform init          # connects to the S3 backend (fails for any account but 765578795051)
cp terraform.tfvars.example terraform.tfvars   # then set staging_access_cidrs
```

## 2. Session pre-flight (every session)

```bash
aws sts get-caller-identity          # must be account 765578795051, the deployment principal
curl -4 https://checkip.amazonaws.com   # still the IP in terraform.tfvars? update if not
git status --porcelain               # must be empty — images are built from a clean commit
deploy/aws/scripts/verify-teardown.sh   # read-only: confirms nothing is left from last time
```

## 3. Create + deploy (`deploy.sh up`)

```bash
deploy/aws/scripts/deploy.sh up
```

Each `terraform apply` inside is interactive: review every plan and type
`yes`. The script never passes `-auto-approve`. It runs these phases:

1. `terraform apply -target=module.ecr`: the three ECR repositories only.
2. Build and push `platform-api` as `<repo>:<full HEAD SHA>`
   (`--platform linux/amd64`; skipped if that immutable tag already exists).
3. Full `terraform apply`: VPC, subnets, IGW, security groups, RDS 16.15,
   secrets, IAM roles, internal ALB, CloudFront VPC origin + 3
   distributions + allowlist function, ECS cluster/services/task
   definitions, alarms. **Expect 20–40 minutes**, mostly RDS (~5–10 min)
   and the CloudFront VPC origin/distributions (~10–20 min). The frontend
   services retry image pulls until phase 4.
4. Build and push `b2c`/`backoffice` with `VITE_API_BASE_URL` = the API's
   CloudFront URL, which exists only after phase 3.
5. Run `role-init` then `migrate` as one-off Fargate tasks. Order matters:
   migrate chains the `schema_migrations` write-revoke itself. Then force a
   new deployment, wait for `services-stable`, and `curl` `/healthz` and
   `/readyz` through CloudFront.

Outputs (`deploy.sh status`): `staging_url` (B2C), `backoffice_url`,
`api_url`, all `https://<id>.cloudfront.net`, reachable only from
`staging_access_cidrs`. Everyone else gets 403.

## 4. Acceptance

- **Smoke**: `curl -fsS "$(terraform output -raw api_url)/readyz"` → 200.
  From a non-allowlisted IP the same URL must return **403** (verify once
  from, e.g., a phone on mobile data).
- **Browser**: open `staging_url` and `backoffice_url` directly. The TLS
  certificate is CloudFront's own (`*.cloudfront.net`), so there is no
  warning. Seed a staff user with `cmd/seed-admin` (run it as a one-off
  task or through the documented seed path) before the Back Office login.
- **Stage 9.3/9.4 acceptance flows** (registration → activation seam →
  deposit via `simulate-callback` → casino play simulation → withdrawal
  four-eyes → Back Office audit/ledger views) run against `api_url` from
  an allowlisted machine. The test-support routes are live only because
  `APP_ENV=staging` and `TEST_SUPPORT_ENDPOINTS_ENABLED=true`, and they
  are reachable only through the allowlisted edge.
- **Logs**: CloudWatch log groups `/ecs/igaming-staging/{platform-api,b2c,backoffice,migrate,role-init}`.

## 5. Temporary multi-replica test

```bash
deploy/aws/scripts/deploy.sh scale 2     # interactive apply; waits for services-stable
# run the multi-replica acceptance (e.g. request an activation token and confirm it
# repeatedly — requests alternate across both tasks behind the ALB — plus the
# deposit/casino flows whose references must stay unique across replicas)
deploy/aws/scripts/deploy.sh scale 1     # back to one replica
```

The first platform-api task is always on-demand `FARGATE`; the second runs
on `FARGATE_SPOT` (+~$0.009/hour including its public IPv4). `deploy.sh up`
always re-applies `PLATFORM_API_DESIRED_COUNT` (default 1).

## 6. Teardown and verification

```bash
deploy/aws/scripts/deploy.sh down     # interactive terraform destroy, then verify-teardown.sh
```

- Expect 15–30 minutes. CloudFront distributions are disabled before
  deletion, and the VPC origin must finish deleting before its subnets can
  go. If the destroy stops on a `DependencyViolation` for a subnet or
  security group, wait a few minutes and run `deploy.sh down` again.
- No manual cleanup should be needed:
  - Secrets are force-deleted (recovery window 0), so their names are
    reusable at once.
  - ECR repositories are force-deleted with their images.
  - RDS: no final snapshot, and automated backups are deleted.
  - Log groups, alarms, IAM roles and CloudFront resources are all
    Terraform-managed.
  - The RDS-managed master secret is removed by RDS with the instance.
- `verify-teardown.sh` (read-only) exits 0 only if nothing named or tagged
  `igaming-staging` remains: RDS instances, backups and snapshots, load
  balancers, NAT gateways, Elastic IPs, ECS clusters, CloudFront
  distributions/VPC origins/functions, secrets (including those scheduled
  for deletion), ECR repositories, log groups, KMS aliases, VPCs, alarms,
  SNS topics, IAM roles, plus a tag sweep in eu-central-1 and us-east-1.
  The tagging API can lag deletions by a few minutes, so re-run it if a
  just-deleted ARN appears.

**Remains after teardown (by design):**

| Item | Cost |
|---|---|
| S3 state bucket (+ ≤ 90 days of old state versions) | well under $0.01/month (state is ~100–500 KB; $0.0235–0.0245/GB-month) |
| Optional cost budget (no actions) | $0 expected (budgets without actions are not charged under AWS Budgets pricing; one budget in any case) |
| Service-linked roles (ECS, ELB, RDS, CloudFront VPC origin), created automatically on first use | free |
| Bootstrap local state file, `terraform.tfvars` on the operator machine | local only |

No NAT Gateway, Elastic IP, customer-managed KMS key, secret, image, log
group or database survives a teardown.

## 7. Cost model (eu-central-1, on-demand list prices, no credits assumed)

Unit prices come from the AWS Pricing API on 2026-09-23, except Fargate
Spot, which is market-priced and not published there: estimated at 30% of
on-demand ("up to 70% off"), with the on-demand worst case shown. CloudFront
data transfer uses the published list price (~$0.085/GB for Europe).

| Component (running, 1 API replica) | $/hour |
|---|---|
| ALB (internal): $0.027/h + LCU (≤ 0.5 LCU at staging traffic, $0.008/LCU-h) | 0.027 + ≤0.004 |
| Public IPv4 ×3 (one per task; $0.005/h each) | 0.015 |
| Fargate platform-api, on-demand, 0.25 vCPU / 0.5 GB ($0.04656/vCPU-h, $0.00511/GB-h) | 0.0142 |
| Fargate b2c + backoffice, Spot (est.) | 0.0085 (on-demand worst case 0.0284) |
| RDS db.t4g.micro single-AZ ($0.019/h) + 20 GB gp3 ($0.137/GB-month) | 0.0228 |
| Secrets Manager ×3 (runtime, JWT, RDS-managed master; $0.40/month each, prorated) | 0.0016 |
| CloudWatch alarms ×9 ($0.10/alarm-month) | 0.0012 |
| **Fixed subtotal** | **≈ 0.094** |
| Usage: CloudWatch Logs ($0.63/GB ingested), CloudFront requests/transfer, ALB LCUs | ≈ 0.002 idle … 0.03 active testing |
| Removed vs Stage 9.3: NAT Gateway + EIP (0.057), 2nd API replica (0.014), Container Insights, SNS, ALB public IPs (0.010), 2 DATABASE_URL secrets, customer-managed KMS key | — |

| Scenario | Estimated cost |
|---|---|
| **A. Per hour while running** | **≈ $0.10** (range $0.095–0.13; +$0.02 if Spot were priced at on-demand) |
| **B. 4-hour acceptance session** | **≈ $0.40–0.55**, plus ≈ $0.05–0.10 per create/destroy cycle (resources partly exist for ~1 hour across create + destroy; the ALB bills per started hour) |
| **C. 8 hours** | **≈ $0.80–1.05** (+ cycle ≈ $0.05–0.10) |
| **D. 24 hours** (mostly idle) | **≈ $2.30–2.60** |
| **E. Accidentally left running 30 days** (730 h, idle) | **≈ $70–75** (up to ≈ $85 if Spot ran at on-demand prices), vs ≈ $135–155 for the Stage 9.3 design |
| Multi-replica test | +≈ $0.009 per hour while 2 replicas run |
| **F. After teardown** | **≈ $0.00–0.01/month** (state bucket; budget free) |

The largest remaining line items are the ALB ($0.027/h), RDS ($0.023/h)
and Fargate (≈$0.023/h). Short sessions are cheap because nothing
continuous survives `down`. The budget alert (§1.2) is the backstop
against scenario E.

## 8. Credits and Free Tier (not assumed anywhere above)

The account's actual charges may be lower if it is on AWS's credit-based
Free Tier (accounts created after mid-2025 receive promotional credits) or
has other credits. Some services also have always-free or 12-month
allowances that can cover this staging footprint:
- CloudFront (1 TB and 10M requests/month always free)
- CloudWatch (10 alarms, 5 GB logs)
- Secrets Manager (30-day trial per new secret)
- RDS / ALB / public IPv4 free-hour allowances under the legacy 12-month
  Free Tier

Whether any of these apply depends on the account's plan. Check **Billing
→ Credits** and **Billing → Free Tier** in the console. None of it was
used to justify a design decision.

## 9. Troubleshooting

- **403 from every URL**: your IP changed or is IPv6-only. Update
  `staging_access_cidrs` and run `deploy.sh up`, which re-applies the
  function; the image build steps skip because the tags already exist.
- **Frontend down for ~1–2 minutes**: a Spot interruption. ECS replaces the
  task automatically. If Spot capacity is unavailable for a long time,
  temporarily set that service's strategy to `FARGATE` in `main.tf`.
- **`Error acquiring the state lock`**: another apply is running, or a
  crashed run left `staging/terraform.tfstate.tflock`. Confirm that nobody
  else is applying, then `terraform force-unlock <ID>`.
- **Service stuck with 0 running tasks after `up`**: check the service
  events (`aws ecs describe-services`) and the log group. Typical causes are
  an image built for the wrong architecture (the script forces amd64) or
  migrations not yet run (`deploy.sh migrate`).

## 10. Rotation

- Runtime DB password / JWT signing secret: bump `secret_version` in
  tfvars, run `deploy.sh up`, then `deploy.sh migrate`. That re-runs
  role-init, which `ALTER ROLE`s the new runtime password, and the services
  redeploy.
- RDS master password: rotated by RDS itself (RDS-managed secret). The
  one-off tasks read it fresh each run.

## 11. Read-only verification before the bucket exists (what was done on 2026-09-23)

The backend points at a bucket that does not exist yet. To `plan` with the
read-only credential without creating anything, run against a **scratch
copy** with a local-backend override file (`*_override.tf` is git-ignored
and must never be committed):

```bash
cp -r deploy/aws /tmp/tfcheck && cd /tmp/tfcheck/aws/environments/staging
printf 'terraform {\n  backend "local" {}\n}\n' > backend_override.tf
terraform init
terraform plan -var "image_tag=$(git -C <repo> rev-parse HEAD)" -var 'staging_access_cidrs=["203.0.113.10/32"]'
```

Offline checks (no AWS at all): `deploy/aws/tests/run-static-checks.sh`
(also the `infrastructure` CI job).
