# Stage 9.4 — AWS Account Verification & Operator Setup

Owner: `devops`. This is the missing piece between `docs/runbooks/
stage-9-3-staging-deployment-runbook.md` (the deployment package itself,
`terraform fmt`/`validate`-clean, never applied) and an actual `terraform
apply`: the exact verification an operator must run against a candidate
AWS account/credential **before** pointing this project's Terraform at
it, and why each check exists. Nothing in this document has been run
against a real account — no authorized AWS credentials have been
supplied to this project as of Stage 9.4.

This is not a general AWS security primer. Every check below exists
because CLAUDE.md's "Environment safety" rule ("Never request or create
access to production credentials... without explicit later
authorization") and Stage 9.3/9.4's own explicit instructions ("Do NOT
use unknown ambient AWS credentials," "Do not use root credentials," "Do
not provision anything in an unknown account") require it — this
document turns those rules into runnable commands with a clear pass/fail
per check, so a future session (or a human operator) does not have to
re-derive the reasoning.

## 0. Before running anything here

Confirm the credential you are about to verify was **explicitly supplied
and authorized for this project** — a value the project owner gave you
on purpose, for this purpose, not a credential that happens to be present
in an environment for an unrelated reason (a CI runner's own deploy
identity, a personal AWS profile, a credential left over from another
project). If that provenance is not clearly established, stop here and
ask, rather than running the checks below against it — a clean
verification result does not establish authorization; it only describes
an account's shape once authorization is already established out of
band.

## 1. Identity verification

```sh
aws sts get-caller-identity
```

Confirm, and record in the deployment log:

- **Account ID** — must match the account the project owner named, not
  merely "an account that exists." An attacker or a stale credential can
  point at a real, working AWS account that is not the intended one.
- **ARN** — must be an IAM **user** or **role**, never `arn:aws:iam::
  <account>:root`. Reject and stop immediately if this is a root
  credential (`aws iam get-account-summary`'s `AccountMFAEnabled`/
  `Arn` fields are a secondary confirmation, but the `get-caller-identity`
  ARN itself is definitive: `root` never appears as a user/role path).
- **UserId** — sanity-check it is not a value you have seen associated
  with a different project (a reused/leaked credential concern, not
  something this check alone can prove, but worth noting if the
  `UserId`/`Account` combination is unexpected).

```sh
aws iam get-user 2>&1 || aws sts get-caller-identity --query Arn
```

If the principal is an IAM role (common for CI/CD or SSO), confirm the
**trust policy** — who/what can assume it — via:

```sh
aws iam get-role --role-name <role-name> --query 'Role.AssumeRolePolicyDocument'
```

and confirm it is scoped to the operator/pipeline actually running this
deployment, not a broad `"Principal": "*"` or an unrelated account.

## 2. Account ownership / billing verification

AWS does not expose "who pays the bill" via a single unauthenticated
API call by design (billing access is itself a permission). Verification
here is necessarily a combination of API checks and an explicit human
confirmation, not API output alone:

```sh
aws organizations describe-account --account-id <account-id> 2>&1
aws iam list-account-aliases
```

- If `describe-account` succeeds, record the account's `Email` and
  `JoinedMethod`/`JoinedTimestamp` — do these match what the project
  owner described (e.g. "an account I created last week under our AWS
  Organization" vs. an account that has existed for years under a
  different name)?
- `list-account-aliases` gives a human-readable account name if one is
  set — cheap corroboration, not proof on its own.
- **The one check that actually matters and that no API call can
  substitute for**: explicit confirmation FROM THE PROJECT OWNER, in
  writing, that this specific account ID is one they own or are
  authorized to provision infrastructure and incur cost in, on this
  project's behalf. Record that confirmation (who gave it, when) in the
  deployment log alongside the account ID above. Do not infer ownership
  from the credential merely working.

## 3. Region confirmation

```sh
aws configure get region 2>&1
echo "AWS_REGION=$AWS_REGION AWS_DEFAULT_REGION=$AWS_DEFAULT_REGION"
```

Confirm the target region matches `deploy/aws/environments/staging/
terraform.tfvars`'s `aws_region` (default `eu-west-1` per `terraform.
tfvars.example` — change it deliberately if the operator's account
should use a different region, e.g. for data-residency reasons already
recorded in `docs/architecture/15-jurisdiction-and-licensing-model.md`,
and note that decision in the deployment log). Never let an ambient
`AWS_REGION`/`AWS_DEFAULT_REGION` environment variable silently pick a
region nobody chose — pass `--region` explicitly on every verification
command in this document if there is any ambiguity.

## 4. Permission verification (least-privilege check, not just "does it work")

Rather than discovering missing permissions one `terraform apply`
failure at a time (which can leave a partially-provisioned, inconsistent
stack), dry-run the exact permission surface `deploy/aws/environments/
staging/`'s Terraform needs, using IAM's own simulator:

```sh
aws iam simulate-principal-policy \
  --policy-source-arn <the ARN from step 1> \
  --action-names \
    ec2:CreateVpc ec2:CreateSubnet ec2:CreateNatGateway ec2:CreateInternetGateway \
    ec2:CreateSecurityGroup ec2:AuthorizeSecurityGroupIngress \
    rds:CreateDBInstance rds:CreateDBSubnetGroup \
    ecr:CreateRepository ecr:PutLifecyclePolicy \
    secretsmanager:CreateSecret secretsmanager:GetSecretValue \
    iam:CreateRole iam:PutRolePolicy iam:PassRole \
    ecs:CreateCluster ecs:RegisterTaskDefinition ecs:CreateService ecs:RunTask \
    elasticloadbalancing:CreateLoadBalancer elasticloadbalancing:CreateTargetGroup \
    logs:CreateLogGroup cloudwatch:PutMetricAlarm \
    acm:RequestCertificate route53:ChangeResourceRecordSets
```

Every action should report `allowed`. A `PassRole` denial is worth
flagging specifically — it is the single most common reason a
Terraform-driven ECS/IAM deployment fails partway through with a
confusing error, since it is required to hand the ECS execution/task
roles to the ECS service itself, not merely to create them.

If the operator's principal has `AdministratorAccess` (common for a
freshly-created staging/sandbox account), this check still has value:
run it anyway and record that the principal is broader than strictly
necessary — that is an accepted, explicit choice for a throwaway staging
account, not a silent assumption, and should be revisited before this
account (or its credential shape) is ever reused for anything closer to
production.

## 5. Intended-scope confirmation

Before `terraform apply`, confirm out loud (record in the deployment
log) that everyone involved agrees on:

1. **This provisions STAGING ONLY** — `deploy/aws/environments/staging/`,
   nothing else. There is no `environments/production` directory in this
   repository; nothing here can accidentally provision production
   infrastructure because it does not exist yet.
2. **Expected resources and rough cost** — a `terraform plan` (not
   `apply`) run first, its output reviewed line by line for anything
   unexpected (an unexpectedly large instance class, an unexpected
   `count`, a resource in the wrong region) before ever running `apply`.
   Staging-sized defaults per ADR 0084: `db.t4g.micro` RDS, Fargate tasks
   at minimal CPU/memory, a single shared NAT Gateway, `desired_count=2`
   for platform-api (see Part 1's activation-seam fix, which this exact
   replica count is meant to exercise correctly), `desired_count=1` for
   each frontend.
3. **No real-money integration, no production data** — confirmed by
   inspecting `terraform plan`'s output for the ECS task definitions'
   environment: `APP_ENV=staging` (never `production`), the mock
   payment/casino/KYC provider registrations unchanged from `cmd/
   platform-api/main.go`, `TEST_SUPPORT_ENDPOINTS_ENABLED=true` (Part 1's
   new explicit opt-in, staging-only).
4. **Who can tear it down, and how** — `terraform destroy` against the
   same state, by the same verified principal; see the deployment
   runbook's own "Tearing it down" section. Staging should be treated as
   fully disposable.

## 6. What to do if any check fails, or no credential is supplied at all

Per Stage 9.4's own Part 2 instruction: **do not proceed to Part 3
(provisioning) and do not fabricate or infer a passing result.** Record
exactly which check failed (or that no credential was supplied at all)
in the Stage 9.4 completion report's "remaining production blockers" /
"infrastructure" section, and stop. The Terraform package itself
(`deploy/aws/`), already `fmt`/`validate`-clean, remains ready to apply
the moment a verified, authorized credential passing every check above
is available — nothing about this document blocks that from happening
quickly once ownership is actually established.

## 7. One-time operator setup checklist (summary, for a person doing this by hand)

1. Confirm you own or are authorized to provision AWS resources, on this
   project's behalf, in a specific account (§2).
2. `aws configure` (or SSO login) with a non-root IAM user/role
   credential for that account (§1).
3. Run every command in §1–§4 above; resolve any `denied` result by
   attaching the missing permission (or granting broader access
   deliberately, per §4's note) before proceeding.
4. Pick/confirm the target region (§3) and, if you want real HTTPS, a
   domain + Route53 hosted zone (see the deployment runbook's §5 — both
   `domain_name` and `route53_zone_id` are optional and independently
   safe to leave unset for the plain-HTTP ALB-DNS-name fallback).
5. `cd deploy/aws/environments/staging && terraform init && terraform
   plan` — review the plan in full (§5.2 above) before `terraform
   apply`.
6. Follow the deployment runbook's §3 step-by-step from there (build/push
   images, `apply`, run the one-off migration/role-init tasks, verify
   `/healthz`/`/readyz`).
