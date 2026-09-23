#!/usr/bin/env bash
# READ-ONLY post-teardown verification for the ephemeral staging
# environment (ADR 0086, docs/runbooks/stage-9-4-staging-lifecycle-runbook.md §6).
# Uses only Describe/List/Get calls — safe to run with a read-only
# credential, at any time (it is also how "nothing is running" is proven
# before and after a session).
#
# Exit 0: no billable staging resource remains (only the intentionally
#         persistent bootstrap items: the state bucket and, if configured,
#         the cost budget).
# Exit 1: something named/tagged for igaming-staging still exists.
#
# Usage: deploy/aws/scripts/verify-teardown.sh
# Env:   NAME_PREFIX (default igaming-staging)
set -uo pipefail

REGION="eu-central-1"
PREFIX="${NAME_PREFIX:-igaming-staging}"
FOUND=0

command -v aws >/dev/null 2>&1 || { echo "ERROR: aws CLI is required" >&2; exit 2; }
command -v jq >/dev/null 2>&1 || { echo "ERROR: jq is required" >&2; exit 2; }

check() { # check "<label>" <command...>  — command prints one line per leftover
  local label="$1"; shift
  local out
  if ! out="$("$@" 2>&1)"; then
    echo "  [ERROR] ${label}: ${out}"
    FOUND=1
    return
  fi
  out="$(echo "${out}" | sed '/^\s*$/d;/^None$/d')"
  if [[ -n "${out}" ]]; then
    echo "  [LEFT]  ${label}:"
    echo "${out}" | sed 's/^/            /'
    FOUND=1
  else
    echo "  [ok]    ${label}: none"
  fi
}

account="$(aws sts get-caller-identity --query Account --output text)" || exit 2
echo "Account ${account}, region ${REGION}, prefix ${PREFIX}"
echo

echo "Continuously billed while present:"
check "RDS instances" aws rds describe-db-instances --region "${REGION}" \
  --query "DBInstances[?starts_with(DBInstanceIdentifier, '${PREFIX}')].[DBInstanceIdentifier,DBInstanceStatus]" --output text
check "RDS retained automated backups" aws rds describe-db-instance-automated-backups --region "${REGION}" \
  --query "DBInstanceAutomatedBackups[?starts_with(DBInstanceIdentifier, '${PREFIX}')].[DBInstanceIdentifier,Status]" --output text
check "RDS manual snapshots" aws rds describe-db-snapshots --region "${REGION}" --snapshot-type manual \
  --query "DBSnapshots[?starts_with(DBInstanceIdentifier, '${PREFIX}')].DBSnapshotIdentifier" --output text
check "Load balancers" aws elbv2 describe-load-balancers --region "${REGION}" \
  --query "LoadBalancers[?starts_with(LoadBalancerName, '${PREFIX}')].[LoadBalancerName,State.Code]" --output text
check "NAT gateways (not deleted)" aws ec2 describe-nat-gateways --region "${REGION}" \
  --filter "Name=tag:Project,Values=igaming-platform" "Name=state,Values=pending,available,deleting,failed" \
  --query "NatGateways[].[NatGatewayId,State]" --output text
check "Elastic IPs (tagged)" aws ec2 describe-addresses --region "${REGION}" \
  --filters "Name=tag:Project,Values=igaming-platform" --query "Addresses[].[AllocationId,PublicIp]" --output text
check "ECS clusters (active)" aws ecs list-clusters --region "${REGION}" \
  --query "clusterArns[?contains(@, ':cluster/${PREFIX}')]" --output text
check "CloudFront distributions" aws cloudfront list-distributions \
  --query "DistributionList.Items[?starts_with(Comment, '${PREFIX}')].[Id,DomainName,Status]" --output text
check "CloudFront VPC origins" bash -o pipefail -c "aws cloudfront list-vpc-origins --output json \
  | jq -r '.VpcOriginList.Items // [] | .[] | select(.Name | startswith(\"${PREFIX}\")) | [.Id, .Name, .Status] | @tsv'"
# Our secrets are named "<prefix>/..."; the RDS-managed master secret is
# named "rds!db-<uuid>" and is identified by the aws:rds:primaryDBInstanceArn
# tag RDS puts on it.
check "Secrets (incl. scheduled for deletion)" bash -o pipefail -c "aws secretsmanager list-secrets --region ${REGION} \
  --include-planned-deletion --output json \
  | jq -r '.SecretList[] | select((.Name | startswith(\"${PREFIX}/\")) or ((.Name | startswith(\"rds!\")) and ([.Tags[]? | select(.Key == \"aws:rds:primaryDBInstanceArn\" and (.Value | contains(\":db:${PREFIX}\")))] | length > 0))) | [.Name, (.DeletedDate // \"active\" | tostring)] | @tsv'"

echo
echo "Storage-billed while present:"
check "ECR repositories" aws ecr describe-repositories --region "${REGION}" \
  --query "repositories[?starts_with(repositoryName, '${PREFIX}/')].repositoryName" --output text
check "CloudWatch log groups" aws logs describe-log-groups --region "${REGION}" \
  --log-group-name-prefix "/ecs/${PREFIX}" --query "logGroups[].logGroupName" --output text
check "KMS aliases" aws kms list-aliases --region "${REGION}" \
  --query "Aliases[?starts_with(AliasName, 'alias/${PREFIX}')].AliasName" --output text

echo
echo "Free or near-free, but must not linger:"
check "VPCs (tagged)" aws ec2 describe-vpcs --region "${REGION}" \
  --filters "Name=tag:Project,Values=igaming-platform" --query "Vpcs[].[VpcId,CidrBlock]" --output text
check "CloudWatch alarms" aws cloudwatch describe-alarms --region "${REGION}" \
  --alarm-name-prefix "${PREFIX}" --query "MetricAlarms[].AlarmName" --output text
check "SNS topics" aws sns list-topics --region "${REGION}" \
  --query "Topics[?contains(TopicArn, ':${PREFIX}-')].TopicArn" --output text
check "CloudFront functions" aws cloudfront list-functions \
  --query "FunctionList.Items[?starts_with(Name, '${PREFIX}')].Name" --output text
check "IAM roles" aws iam list-roles \
  --query "Roles[?starts_with(RoleName, '${PREFIX}-')].RoleName" --output text

# CloudFront VPC origins create AWS-managed ENIs and a service-managed
# security group ("CloudFront-VPCOrigins-Service-SG") inside the VPC; they
# are not Terraform-managed. If either lingers, VPC deletion fails with
# DependencyViolation (runbook §6 troubleshooting).
if ! staging_vpcs="$(aws ec2 describe-vpcs --region "${REGION}" \
  --filters "Name=tag:Project,Values=igaming-platform" "Name=tag:Environment,Values=staging" \
  --query "Vpcs[].VpcId" --output text)"; then
  echo "  [ERROR] staging VPC lookup failed — cannot check network interfaces / security groups"
  FOUND=1
  staging_vpcs=""
fi
staging_vpcs="$(echo "${staging_vpcs}" | tr '\t' ',')"
if [[ -n "${staging_vpcs}" && "${staging_vpcs}" != "None" ]]; then
  check "Network interfaces in staging VPC(s)" aws ec2 describe-network-interfaces --region "${REGION}" \
    --filters "Name=vpc-id,Values=${staging_vpcs}" \
    --query "NetworkInterfaces[].[NetworkInterfaceId,InterfaceType,Description]" --output text
  check "Non-default security groups in staging VPC(s) (incl. CloudFront-VPCOrigins-Service-SG)" \
    aws ec2 describe-security-groups --region "${REGION}" --filters "Name=vpc-id,Values=${staging_vpcs}" \
    --query "SecurityGroups[?GroupName!='default'].[GroupId,GroupName]" --output text
else
  echo "  [ok]    Network interfaces / security groups in staging VPC(s): no staging VPC"
fi

echo
echo "Tag sweep (Project=igaming-platform, Environment=staging; bootstrap Component and ECS task definitions excluded):"
for r in "${REGION}" us-east-1; do
  check "Tagged resources in ${r}" bash -o pipefail -c "aws resourcegroupstaggingapi get-resources --region ${r} \
    --tag-filters Key=Project,Values=igaming-platform Key=Environment,Values=staging --output json \
    | jq -r '.ResourceTagMappingList[] | select(([.Tags[] | select(.Key==\"Component\" and .Value==\"bootstrap\")] | length) == 0) | .ResourceARN | select(contains(\":task-definition/\") | not)'"
done

# Deregistered (INACTIVE) ECS task definitions are free and keep their tags;
# they are reported for information only, never counted as leftovers.
inactive_tds="$(aws ecs list-task-definitions --region "${REGION}" --status INACTIVE \
  --family-prefix "${PREFIX}" --query "length(taskDefinitionArns)" --output text 2>/dev/null || echo "?")"
echo "  [info]  INACTIVE ${PREFIX} task definitions (free, not billable): ${inactive_tds}"

echo
echo "Intentionally persistent (expected to remain; see runbook §6):"
aws s3api list-buckets --query "Buckets[?starts_with(Name, 'igaming-platform-staging-tfstate-')].Name" --output text \
  | sed '/^\s*$/d;s/^/  [kept]  state bucket: /' || true
aws budgets describe-budgets --account-id "${account}" \
  --query "Budgets[?BudgetName=='igaming-platform-account-monthly'].BudgetName" --output text 2>/dev/null \
  | sed '/^\s*$/d;/^None$/d;s/^/  [kept]  budget: /' || true

echo
if [[ "${FOUND}" -eq 0 ]]; then
  echo "RESULT: no billable igaming-staging resources remain."
  echo "(The tagging API can lag deletions by a few minutes — re-run if a just-deleted ARN is listed.)"
  exit 0
fi
echo "RESULT: leftover staging resources found (listed above) — investigate before assuming billing stopped."
exit 1
