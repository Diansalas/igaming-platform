#!/usr/bin/env python3
"""READ-ONLY IAM policy simulation for the staging deployer policies (ADR 0086).

Evaluates the three deploy/aws/iam/staging-deployer-*.json policies with IAM's
SimulateCustomPolicy API (no resources are created or changed) and asserts
that the privilege-escalation paths found in the Stage 9.4 security review
are denied while every operation the staging lifecycle needs is allowed.

Needs boto3 and any credential allowed iam:SimulateCustomPolicy (the
read-only verification credential is enough). Not part of the offline CI
checks because it calls AWS. Run:
    python3 deploy/aws/tests/simulate-deployer-policies.py
Exit status 0 = every expectation held.
"""
import pathlib
import sys

import boto3

ACCOUNT = "765578795051"
REGION = "eu-central-1"
BOUNDARY = f"arn:aws:iam::{ACCOUNT}:policy/igaming-staging-ecs-role-boundary"
TASK_ROLE = f"arn:aws:iam::{ACCOUNT}:role/igaming-staging-ecs-task-role"
STATE = "arn:aws:s3:::igaming-platform-staging-tfstate-765578795051/staging/terraform.tfstate"

IAM_DIR = pathlib.Path(__file__).resolve().parent.parent / "iam"
POLICIES = [
    (IAM_DIR / "staging-deployer-network-policy.json").read_text(),
    (IAM_DIR / "staging-deployer-infra-policy.json").read_text(),
    (IAM_DIR / "staging-deployer-edge-iam-state-policy.json").read_text(),
]


def ctx(**kv):
    return [{"ContextKeyName": k.replace("__", ":").replace("_SLASH_", "/"), "ContextKeyValues": [v], "ContextKeyType": "string"} for k, v in kv.items()]


REGION_CTX = ctx(aws__RequestedRegion=REGION)
STAGING_TAGS = ctx(aws__ResourceTag_SLASH_Project="igaming-platform", aws__ResourceTag_SLASH_Environment="staging")

# (label, action, resource, context, expect_allowed)
CASES = [
    ("CreateRole with the staging boundary", "iam:CreateRole", TASK_ROLE, ctx(iam__PermissionsBoundary=BOUNDARY), True),
    ("CreateRole without a boundary", "iam:CreateRole", TASK_ROLE, None, False),
    ("CreateRole with another boundary", "iam:CreateRole", TASK_ROLE, ctx(iam__PermissionsBoundary=f"arn:aws:iam::{ACCOUNT}:policy/Admin"), False),
    ("CreateRole outside the three staging role names", "iam:CreateRole", f"arn:aws:iam::{ACCOUNT}:role/igaming-staging-x", ctx(iam__PermissionsBoundary=BOUNDARY), False),
    ("PutRolePolicy on a bounded staging role", "iam:PutRolePolicy", TASK_ROLE, ctx(iam__PermissionsBoundary=BOUNDARY), True),
    ("PutRolePolicy on an unbounded role", "iam:PutRolePolicy", TASK_ROLE, None, False),
    ("UpdateAssumeRolePolicy (trust hijack)", "iam:UpdateAssumeRolePolicy", TASK_ROLE, None, False),
    ("DeleteRolePermissionsBoundary", "iam:DeleteRolePermissionsBoundary", TASK_ROLE, None, False),
    ("CreatePolicyVersion on the boundary", "iam:CreatePolicyVersion", BOUNDARY, None, False),
    ("AttachRolePolicy", "iam:AttachRolePolicy", TASK_ROLE, None, False),
    ("CreateAccessKey", "iam:CreateAccessKey", f"arn:aws:iam::{ACCOUNT}:user/anyone", None, False),
    ("PassRole to ecs-tasks", "iam:PassRole", TASK_ROLE, ctx(iam__PassedToService="ecs-tasks.amazonaws.com"), True),
    ("PassRole to lambda", "iam:PassRole", TASK_ROLE, ctx(iam__PassedToService="lambda.amazonaws.com"), False),
    ("Modify a non-staging load balancer", "elasticloadbalancing:ModifyLoadBalancerAttributes", f"arn:aws:elasticloadbalancing:{REGION}:{ACCOUNT}:loadbalancer/app/prod-alb/abc", None, False),
    ("Create the staging load balancer", "elasticloadbalancing:CreateLoadBalancer", f"arn:aws:elasticloadbalancing:{REGION}:{ACCOUNT}:loadbalancer/app/igaming-staging-alb/abc", None, True),
    ("Modify a non-staging DB", "rds:ModifyDBInstance", f"arn:aws:rds:{REGION}:{ACCOUNT}:db:prod-db", None, False),
    ("Modify the staging DB", "rds:ModifyDBInstance", f"arn:aws:rds:{REGION}:{ACCOUNT}:db:igaming-staging-db", None, True),
    ("Delete an untagged security group", "ec2:DeleteSecurityGroup", f"arn:aws:ec2:{REGION}:{ACCOUNT}:security-group/sg-0123", REGION_CTX, False),
    ("Delete a staging-tagged security group", "ec2:DeleteSecurityGroup", f"arn:aws:ec2:{REGION}:{ACCOUNT}:security-group/sg-0123", STAGING_TAGS, True),
    ("Open a foreign security group", "ec2:AuthorizeSecurityGroupIngress", f"arn:aws:ec2:{REGION}:{ACCOUNT}:security-group/sg-0123", REGION_CTX, False),
    ("Add a rule to a staging-tagged security group", "ec2:AuthorizeSecurityGroupIngress", f"arn:aws:ec2:{REGION}:{ACCOUNT}:security-group/sg-0123", STAGING_TAGS, True),
    ("Modify a foreign VPC attribute", "ec2:ModifyVpcAttribute", f"arn:aws:ec2:{REGION}:{ACCOUNT}:vpc/vpc-0123", REGION_CTX, False),
    ("Tag a foreign resource (tag hijack)", "ec2:CreateTags", f"arn:aws:ec2:{REGION}:{ACCOUNT}:security-group/sg-0123", REGION_CTX, False),
    ("Read a staging app secret (provider refresh)", "secretsmanager:GetSecretValue", f"arn:aws:secretsmanager:{REGION}:{ACCOUNT}:secret:igaming-staging/jwt-signing-secret-AbCdEf", None, True),
    ("Read the RDS-managed master secret", "secretsmanager:GetSecretValue", f"arn:aws:secretsmanager:{REGION}:{ACCOUNT}:secret:rds!db-123", None, False),
    ("Delete the state object", "s3:DeleteObject", STATE, None, False),
    ("Delete the state lock file", "s3:DeleteObject", STATE + ".tflock", None, True),
    ("sns:ListTopics (verify-teardown)", "sns:ListTopics", "*", None, True),
    ("Read the AWS-managed CloudFront origin-facing prefix list", "ec2:GetManagedPrefixListEntries", f"arn:aws:ec2:{REGION}:aws:prefix-list/pl-a3a144ca", None, True),
    ("Read a customer-managed prefix list", "ec2:GetManagedPrefixListEntries", f"arn:aws:ec2:{REGION}:{ACCOUNT}:prefix-list/pl-0123", None, False),
]


def main() -> int:
    iam = boto3.client("iam")
    failures = 0
    for label, action, resource, context, expect_allowed in CASES:
        kwargs = {"PolicyInputList": POLICIES, "ActionNames": [action], "ResourceArns": [resource]}
        if context:
            kwargs["ContextEntries"] = context
        decision = iam.simulate_custom_policy(**kwargs)["EvaluationResults"][0]["EvalDecision"]
        ok = (decision == "allowed") == expect_allowed
        failures += 0 if ok else 1
        print(f"{'PASS' if ok else 'FAIL'}  {label:55s} {decision}")
    print(f"\n{len(CASES) - failures}/{len(CASES)} expectations held")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
