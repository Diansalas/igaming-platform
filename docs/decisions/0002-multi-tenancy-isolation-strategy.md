# ADR 0002 — Multi-Tenancy via Shared Cluster + Row-Level Security, With a Tightening Path

Status: Accepted (derived from Blueprint §5)

## Context

The platform must serve our own B2C brand first and, later, B2B partner
brands, without forking the codebase. Tenant isolation must be provably
enforced, not merely conventional, since a leak is both a security and a
regulatory failure.

## Decision

- Start with a shared PostgreSQL cluster. Every tenant-owned table carries
  `tenant_id`. Isolation is enforced by row-level security bound to a
  connection-level tenant setting — never by relying on every query
  remembering `WHERE tenant_id = ?`.
- Everything brand-specific (theme, catalogue, payment methods, currencies,
  languages, RG defaults, bonus templates, jurisdiction rules, provider
  credentials, domains) is a versioned configuration row, never a code
  path. The moment a brand needs a code path, we've started a consulting
  business instead of a platform.
- The data-access layer is designed so tightening isolation later (schema-
  per-tenant → database-per-tenant → cluster-per-tenant) is a deployment/
  connection-routing decision, not an application rewrite.

## Consequences

- A regulated partner or tier-1 jurisdiction can eventually be told "yes,
  we can isolate you further" without a migration project.
- RLS correctness becomes a mandatory, standing test category (see
  `docs/testing/testing-strategy.md`) — every new endpoint needs a
  cross-tenant-access-must-fail test.
- Tenant configuration (`tenant-config` service) becomes a foundational
  Stage 2 deliverable, not a later add-on.

## Owner

`architect`, enforced in review by `security`.
