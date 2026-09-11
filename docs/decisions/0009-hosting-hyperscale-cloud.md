# ADR 0009 — Hosting on a Major Hyperscale Cloud (Supersedes Stage 0 Hosting Recommendation)

Status: Accepted (human decision, resolves Q6 from ADR 0005, supersedes
the hosting row of `0003-technology-stack.md`)

## Context

Stage 0 recommended Hetzner/OVH/Leaseweb-class hosting, flagging that
AWS/GCP acceptable-use policies for gambling vary by region and licence
and must be confirmed in writing before committing. The human has now
directed a major hyperscale cloud architecture instead, prioritizing
managed security/IAM/networking/database/DR/observability capability over
the earlier cost-driven recommendation — subject to the same written AUP
confirmation requirement, which is **not waived** by this decision.

## Decision

Target a major hyperscale cloud provider (AWS, GCP, or Azure — final
provider selection remains open and should be confirmed alongside AUP
verification) with:

- Strong IAM, private networking (VPC/private subnets), managed database
  services, automated backups, documented disaster recovery, centralized
  logging/monitoring, WAF/DDoS protection, and managed secrets (cloud KMS
  or Vault on top of it).
- Regional deployment options to support jurisdiction-specific data
  residency requirements (see `docs/architecture/15-jurisdiction-and-
  licensing-model.md`).
- Reasonable cloud portability: infrastructure-as-code and avoidance of
  proprietary managed services where an equivalent portable option exists,
  so the platform is not irreversibly locked to one vendor.
- Strict environment separation: development, staging, and production are
  separate environments/accounts; production is isolated from development
  and never seeded with production data for development use.

## What this decision does not do

Selecting a reputable hyperscale provider does not by itself make hosting
gambling-compliant. **Before any production deployment**, the following
must be confirmed and documented (`docs/decisions/` — a follow-up ADR when
resolved):

1. The provider's gambling-related acceptable-use policy, in writing.
2. Contractual permission for the specific intended operation (real-money
   gambling, in the relevant jurisdictions).
3. Data residency requirements per jurisdiction actually served.
4. Any other jurisdiction-specific hosting requirements.

Until that confirmation exists, only development/staging environments are
provisioned — no production, no real-money workload.

## Consequences

- `devops` designs Stage 1's environment separation and IaC against a
  hyperscale target from the start (rather than Hetzner-specific tooling),
  while keeping the actual Stage 1 deliverable a local/dev-only setup —
  no cloud account provisioning happens without further authorization.
- `docs/decisions/0003-technology-stack.md`'s hosting row is superseded by
  this ADR; its other rows (Postgres, Go, Redis, etc.) stand.

## Owner

`devops` for implementation; provider AUP confirmation is a human/legal
task, not delegated to any specialist.
