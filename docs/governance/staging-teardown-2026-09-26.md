# Staging teardown record — 2026-09-26

**Authorization:** human instruction "MASTER ORCHESTRATOR — STAGING TEARDOWN + CONTINUE DEVELOPMENT"
(2026-09-26): execute only `./deploy/aws/scripts/deploy.sh down`, verify, document.
**Status: staging OFF.**

| Field | Value |
|---|---|
| Previous staging commit | `9190d5d01da076141a1f70d7e5897a573d3b18f4` (Terraform output `image_tag`, read before the plan) |
| Latest development commit at teardown | `957a3e8f567236bb067b6ea770bd75551c3d0494` (Stage 10.2 complete); `957a3e8` was **not** deployed |
| AWS account | 765578795051 (verified with `aws sts get-caller-identity`) |
| Principal | IAM user `claude-staging-deployer` |
| Region | eu-central-1 |
| Command | `./deploy/aws/scripts/deploy.sh down` (unmodified governed script) |
| Destroy started / finished (UTC) | 2026-09-26T12:14:28Z / 2026-09-26T12:35:18Z |
| Terraform result | `Destroy complete! Resources: 74 destroyed.` |
| Terraform state after | 0 resources (state list empty) |

## Pre-destroy checks (all passed before confirmation)

- Account, principal and region as above; Terraform `allowed_account_ids` also pins 765578795051.
- Remote state (`s3://igaming-platform-staging-tfstate-765578795051/staging/terraform.tfstate`): 84
  entries = 74 managed resources + 10 data sources.
- `terraform plan -destroy`: **0 to add, 0 to change, 74 to destroy** — no ADD, no CHANGE.
- All 74 in the staging modules (alb 8, database 3, ecr 6, ecs 17, edge 5, iam 5, network 12,
  observability 9, secrets 6, security 3); every named resource `igaming-staging*` or tagged
  `Environment=staging`.
- IAM in the plan: only the three Terraform-created staging ECS roles (`igaming-staging-ecs-task-role`,
  `-ecs-service-execution`, `-ecs-one-off-execution`) and their two inline policies — part of the
  disposable environment, as the runbook specifies. No IAM user, access key, managed policy, permission
  boundary, analyzer, budget or S3 bucket was in the plan.
- Database: `igaming-staging-db`, `deletion_protection=false`, `skip_final_snapshot=true`,
  `delete_automated_backups=true` (by design; data was synthetic only).
- Terraform's interactive confirmation was answered `yes` by a driver only after the printed plan line
  matched `Plan: 0 to add, 0 to change, 74 to destroy.` exactly (it would otherwise have answered `no`).

## Destroyed (74)

ECS cluster, capacity providers, 3 services, 6 task definitions; ALB, listener, 3 listener rules, 3 target
groups; 3 CloudFront distributions, CloudFront VPC origin, CloudFront function; RDS instance, subnet
group, parameter group; 3 ECR repositories + lifecycle policies; 3 Secrets Manager secrets + versions;
6 log groups, 9 alarms; VPC, 4 subnets, 2 route tables, 4 associations, internet gateway, 3 security
groups; 3 staging IAM roles + 2 inline policies.

## Verification (`verify-teardown.sh`, read-only)

First run (immediately after destroy): every category **ok / none** — RDS instances, automated backups,
manual snapshots, load balancers, NAT gateways, Elastic IPs, active ECS clusters, CloudFront
distributions, VPC origins, functions, secrets (including scheduled deletion), ECR repositories, log
groups, KMS aliases, VPCs, alarms, SNS topics, IAM roles, staging-VPC ENIs/security groups, us-east-1
tag sweep — **except** the eu-central-1 tag sweep, which still listed the ECS cluster and its three
services. Direct `ecs describe-clusters` / `describe-services` returned all four as **`INACTIVE`**
(deleted; ECS retains inactive records for a period at no cost); the runbook (§6) notes the tagging API
lags deletions. Re-run result: see "Final verification" below.

## Final verification

`verify-teardown.sh` was re-run every minute for 30 attempts (last run 2026-09-26T13:23:17Z). **It still
exits 1**, for one reason only: the eu-central-1 tag sweep (Resource Groups Tagging API) keeps indexing
the ECS cluster `igaming-staging-cluster` and its three services. All 20 other categories report
`none`, and the us-east-1 sweep is clean.

Direct ECS evidence (read-only), same time:

| Object | Status | Running / pending tasks | Desired |
|---|---|---|---|
| cluster `igaming-staging-cluster` | `INACTIVE` (absent from `list-clusters`) | 0 / 0, 0 active services | — |
| service `igaming-staging-platform-api` | `INACTIVE` | 0 | 0 |
| service `igaming-staging-b2c` | `INACTIVE` | 0 | 0 |
| service `igaming-staging-backoffice` | `INACTIVE` | 0 | 0 |

`INACTIVE` is ECS's deleted state; ECS keeps the records (and the tagging index keeps their ARNs) for a
while, and neither is billable — the same reason the script already treats INACTIVE task definitions as
`[info]`, not leftovers. **Disposition: teardown complete; the verify exit code is 1 because of this
indexing lag, not because a resource exists.** Registered as VERIFY-TEARDOWN-ECS-1 (devops): the tag
sweep should exclude INACTIVE ECS clusters/services as it excludes task definitions. `deploy/` was not
changed now (outside this authorization). Re-running `verify-teardown.sh` later should return 0 once AWS
purges the INACTIVE records.

## Retained by design (runbook §6)

| Item | Status |
|---|---|
| Terraform state bucket `igaming-platform-staging-tfstate-765578795051` | present (verified) |
| IAM permission boundary `igaming-staging-ecs-role-boundary` | present, 0 attachments (verified) |
| Cost budget | 1 budget present (verified) |
| Deployer IAM user `claude-staging-deployer` | present (it authenticated every call) |
| Service-linked roles, IAM Access Analyzer account analyzer, AWS-managed keys | not modified (outside the plan) |

## Not performed

- **Runbook §6 session-end Access Analyzer check:** the deployer credential is not permitted
  `access-analyzer:ListAnalyzers` / `ListFindings` (least privilege). Not run; **the human (account
  admin) should run it**. Mitigating fact: all three staging IAM roles were destroyed and verified gone,
  so no staging role remains that could be externally trusted.

## Safety confirmations

- Only `deploy.sh down` was executed against AWS; every other AWS call was read-only (STS, describe,
  list, head, get-policy, the verify script).
- No manual resource deletion; no Terraform code, IAM, user, credential or production change; no
  credential rotation.
- `957a3e8` was not deployed. No application code was changed; the repository working tree stayed clean.
- No secret, password, token or access key is recorded here. Terraform state was never copied into the
  repository.

## Tooling note

The container had no Terraform/AWS CLI; Terraform 1.16.4 (checksum-verified from releases.hashicorp.com)
and AWS CLI v2 were installed into the session scratchpad. `registry.terraform.io` is blocked by this
environment's network policy; it was not routed around — Terraform used a provider mirror already on
disk from the Stage 9.4 session, verified by Terraform against `.terraform.lock.hcl`
(aws 5.100.0, random 3.9.1).

## Next staging deployment

Per the human's instruction, there will be **one** deliberate governed staging deployment from the final
approved commit (`deploy.sh up`, fresh environment, all migrations, full acceptance). It is not scheduled
and needs separate human authorization. Until then staging is OFF and items needing AWS are marked
**STAGING REQUIRED** (`docs/plans/stage-10.3-planning/00-roadmap-reconciliation.md` §8).
