# Stage 9.3 — Staging AWS Deployment Runbook

Owner: `devops`. Companion to
`docs/decisions/0084-stage-9-3-staging-aws-architecture.md` (the ADR
recording this as a scoped STAGING decision under ADR 0009) and
`docs/architecture/38-deployment-architecture.md` (the deployment contract
this package implements). Terraform code lives under `deploy/aws/`.

**Nothing in this package has been applied against a real AWS account.**
No `terraform plan`/`apply` was run while building it, and this session
never read or used the ambient `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`
already present in its shell — those were explicitly flagged as
unconfirmed (not verified as belonging to this project, billing not
confirmed) and must not be used. Everything below is written for a
**future operator** holding real, confirmed AWS credentials.

## 1. Prerequisites (the actual blocker this whole package sits behind)

1. **A confirmed, billed AWS account the human/org actually owns.** This
   is the one-time operator action this entire package is blocked on. Do
   not reuse an ambient/unverified credential from an unrelated context —
   confirm account ownership and billing first.
2. An IAM principal (user or role) able to create: VPCs/subnets/NAT/IGW,
   security groups, RDS instances + parameter/subnet groups, KMS keys, ECR
   repositories, Secrets Manager secrets, IAM roles/policies, ECS
   clusters/task definitions/services, ALB + target groups + listeners,
   ACM certificates, Route53 records (if using a domain), CloudWatch log
   groups/alarms, SNS topics. (Full `AdministratorAccess` is the simplest
   bootstrap credential for a throwaway staging account; a narrower
   policy is fine too — this package does not require it to be broad.)
3. Terraform >= 1.9.
4. Docker (for building the 3 images).
5. AWS CLI v2, configured with credentials for the account in (1)
   (`aws configure` or `aws sso login`).
6. `jq` (used by `deploy/aws/scripts/deploy.sh`).
7. Optional: a domain name and a Route53 hosted zone for it, if you want a
   real HTTPS staging URL instead of the plain-HTTP ALB-DNS-name fallback
   (see §5).

## 2. What this package validated, and how (no AWS calls)

From `deploy/aws/environments/staging`:

```
terraform init -backend=false
terraform fmt -check -recursive   # run from repo root against deploy/aws
terraform validate
```

All three passed cleanly. `terraform plan`/`apply` were deliberately never
run. The `.terraform`/`.terraform.lock.hcl` produced by this validation
were removed afterward — run your own `terraform init` (with real registry
access) before you `apply`; it will generate its own lock file with
registry-verified checksums.

## 3. Step-by-step: from this package to a reachable staging URL

```bash
cd deploy/aws/environments/staging
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars if you want a custom domain (see §5) or different
# sizing. There are no secrets to fill in — Terraform generates all of
# them.

terraform init
terraform plan     # review what will be created
terraform apply    # confirm — this is the first real AWS API call this package makes
```

This creates the VPC, RDS instance, ECR repos, Secrets Manager secrets,
IAM roles, ECS cluster + task definitions (with placeholder/no images
pushed yet — the services will show 0 running tasks or fail to pull an
image until the next step), the ALB, and the observability alarms/SNS
topic.

```bash
cd ../../scripts
IMAGE_TAG=$(git rev-parse --short HEAD) ./deploy.sh
```

`deploy.sh` does, in order:

1. Reads Terraform outputs (ECR repo URLs, cluster name, subnets, security
   group, task definition ARNs).
2. Logs into ECR and builds+pushes the 3 images: `platform-api` from
   `deploy/docker/platform-api.Dockerfile`, and `b2c`/`backoffice` from
   the shared, parameterized `deploy/docker/frontend.Dockerfile`
   (`--build-arg APP_DIR=b2c` or `backoffice` — see
   `deploy/docker/README.md`).
3. Re-runs `terraform apply -var image_tag=<tag>` so the ECS task
   definitions point at the freshly pushed images.
4. Runs the **role-init** one-off task, waits for it to stop, and fails
   loudly (non-zero exit, script aborts) if its container exited non-zero.
5. Runs the **migrate** one-off task the same way. Its own container
   command is `/app/migrate up && psql "$DATABASE_URL" -c "REVOKE
   INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime;"` —
   both halves run as the RDS master (`igaming`) credential, in the same
   task invocation, so the revoke can never be silently skipped by a
   separate step being forgotten.
6. Forces a new deployment of the 3 long-running ECS services so they
   pick up the new task definition revision (belt-and-braces on top of
   step 3, which already changes the revision when the image tag
   changes).

**Ordering matters and is enforced by the script, not by Terraform**
(one-off `run-task` invocations are an operator/pipeline action, outside
Terraform's dependency graph): role-init MUST complete before migrate.
`schema_migrations` does not exist until migrate creates it, so
role-init's own `ALTER DEFAULT PRIVILEGES ... ON TABLES` legitimately (and
correctly, for every other table) also grants `igaming_runtime` write
access to `schema_migrations` the moment it's created — which is why the
revoke has to be a distinct, later step, chained onto the end of migrate's
own command, not folded into role-init. This exact ordering mirrors
`.github/workflows/ci.yml`'s own three-step sequence for the identical
reason; re-read `deploy/aws/sql/init-runtime-role.rds.sql`'s header
comment before changing this if you ever touch it, so a future
credential-rotation re-run doesn't silently regress it.

```bash
cd ../environments/staging
terraform output staging_url
terraform output backoffice_url
terraform output api_url
```

Confirm the deployment is actually healthy:

```bash
curl -sS "$(terraform output -raw api_url)/healthz"   # liveness — should be 200 immediately
curl -sS "$(terraform output -raw api_url)/readyz"    # readiness — 200 once DB is reachable
```

If `domain_name`/`route53_zone_id` were not supplied (see §5), `api_url`/
`staging_url`/`backoffice_url` all resolve to the same plain-HTTP ALB DNS
name — use a `Host` header override to reach a specific service:

```bash
ALB_DNS=$(terraform output -raw alb_dns_name)
curl -sS -H "Host: api.staging.internal" "http://${ALB_DNS}/healthz"
curl -sS -H "Host: app.staging.internal" "http://${ALB_DNS}/"
curl -sS -H "Host: admin.staging.internal" "http://${ALB_DNS}/"
```

### Manually re-running the one-off tasks (without the wrapper script)

If you need to re-run migrate or role-init directly (e.g. after a schema
change, or to rotate `igaming_runtime`'s password following a Secrets
Manager rotation):

```bash
CLUSTER=$(terraform output -raw ecs_cluster_name)
SUBNETS=$(terraform output -json private_subnet_ids)
SG=$(terraform output -raw ecs_security_group_id)
NET_CONFIG=$(jq -n --argjson subnets "${SUBNETS}" --arg sg "${SG}" \
  '{awsvpcConfiguration:{subnets:$subnets, securityGroups:[$sg], assignPublicIp:"DISABLED"}}')

# role-init
aws ecs run-task --cluster "${CLUSTER}" \
  --task-definition "$(terraform output -raw ecs_role_init_task_definition_arn)" \
  --launch-type FARGATE --network-configuration "${NET_CONFIG}"

# migrate (chains the schema_migrations revoke itself — see above)
aws ecs run-task --cluster "${CLUSTER}" \
  --task-definition "$(terraform output -raw ecs_migrate_task_definition_arn)" \
  --launch-type FARGATE --network-configuration "${NET_CONFIG}"
```

Check the corresponding CloudWatch log group
(`/ecs/igaming-staging/role-init` or `/ecs/igaming-staging/migrate`) for
output/errors — `aws ecs describe-tasks` on the returned task ARN shows
the container exit code.

## 4. Tearing it down

Staging is fully disposable by design (`deletion_protection=false`,
`skip_final_snapshot=true`, no persistent state outside RDS/ECR that
matters once the environment is gone):

```bash
cd deploy/aws/environments/staging
terraform destroy
```

This deletes everything, including the RDS instance (no final snapshot is
kept — if you need to preserve data from a staging investigation, take a
manual `aws rds create-db-snapshot` first). ECR repositories are deleted
along with their images; nothing needs manual cleanup afterward.

## 5. The domain / TLS decision (genuinely optional, not fabricated)

Leave `domain_name` and `route53_zone_id` unset in `terraform.tfvars` to
get a working staging environment reachable over plain HTTP on the ALB's
own `*.elb.amazonaws.com` name (see §3's `Host` header technique). This is
explicitly a **temporary, non-HTTPS fallback** — labeled as such in
`terraform output alb_dns_name_fallback_warning` and here — chosen because
fabricating a domain decision was explicitly out of scope for this
package.

Supply BOTH `domain_name` (an apex domain you control) and
`route53_zone_id` (its Route53 hosted zone ID) together to get:

- `api-staging.<domain>`, `app-staging.<domain>`, `admin-staging.<domain>`
- A real ACM certificate (DNS-validated) and an HTTPS listener on the ALB
- Route53 alias A records pointing each hostname at the ALB
- The B2C/Back Office frontends can then genuinely call the API
  cross-origin, since `CORS_ALLOWED_ORIGINS` is computed from these exact
  hostnames automatically.

Without a domain, the frontends' `VITE_API_BASE_URL` build arg has no
real, resolvable absolute URL to point at — the plain-HTTP fallback is
good for infrastructure smoke-testing (health checks, ECS wiring, RDS
connectivity, log/alarm plumbing) but not for a realistic cross-origin
frontend demo. That requires the domain.

## 6. Promoting to production

See `docs/decisions/0084-stage-9-3-staging-aws-architecture.md`'s
"Promoting to production" section for the full, named list. Summary:
multi-AZ RDS, `deletion_protection=true`, bigger instance/task sizes, one
NAT Gateway per AZ, a WAF in front of the ALB, a real domain (not the
fallback), longer log/backup retention, alarms wired to a real on-call
channel (not just an optional email), `APP_ENV=production` set only
there — and, blocking all of it, ADR 0009's still-open gambling AUP/legal
confirmation, which is a human/legal task, not something any engineering
session can close.

## 7. Every variable an operator must supply or review before `apply`

None are required to have a non-default value for a first apply — every
variable in `deploy/aws/environments/staging/variables.tf` has a
staging-sized default. Review, at minimum:

| Variable | Default | Why you might change it |
|---|---|---|
| `aws_region` | `eu-west-1` | Pick the region matching your confirmed account/data residency intent. |
| `domain_name` / `route53_zone_id` | `null` / `null` | Set both together for HTTPS (§5); leave both unset for the plain-HTTP fallback. |
| `alarm_email` | `null` | Set to get CloudWatch alarm emails; staging works fine without it. |
| `db_instance_class` | `db.t4g.micro` | Bump if staging load requires it. |
| `image_tag` | `latest` | `deploy.sh` overrides this per-run with a git SHA; only relevant for manual applies. |

No variable holds a secret — there is nothing else to fill in.

## 8. Known limitations of this staging package (read before assuming more than is here)

- Single NAT Gateway (cost tradeoff, named explicitly in
  `deploy/aws/modules/network`) — not a production HA pattern.
- No WAF in front of the ALB.
- `terraform.tfstate` is local by default (`deploy/aws/environments/staging/backend.tf`
  has a commented S3 backend example) — switch to a remote backend before
  more than one person operates this environment.
- The non-HTTPS fallback (no domain) is explicitly not meant to carry a
  real cross-origin frontend demo — see §5.
- This package was built and statically validated (`init -backend=false`,
  `fmt -check`, `validate`) but never planned or applied against a real
  account. The first real `terraform plan` may surface issues static
  validation cannot catch (IAM permission gaps on the operator's own
  credential, region-specific AZ/instance-class availability, etc.) —
  budget time for that on the first real run.
