# ADR 0006 — Hybrid Licensing Model, Jurisdiction as First-Class Config

Status: Accepted (human decision, resolves Q2 from ADR 0005)

## Context

Stage 0 flagged "do partners sit under our licence, or bring their own?"
as the single highest-leverage open commercial question, because it
determines credential ownership, cost attribution, and tenant-isolation
strictness.

## Decision

Both models are supported simultaneously, selected per tenant:

- Tenants operating under **our own gambling licence** (starting with
  Anjouan).
- Tenants operating under **their own gambling licence**, in their own
  jurisdiction.

Licensing/jurisdiction is modeled as first-class, configuration-driven
data (`Jurisdiction`, `Licence`, `Tenant.licensing_model`,
`TenantJurisdictionConfig` — see `docs/architecture/15-jurisdiction-and-
licensing-model.md`), never a single global assumption and never a code
path per licensing model.

Target markets for commercial launch: **Europe and LATAM**, each modeled
as multiple distinct `Jurisdiction` rows with their own regulatory
configuration — never treated as one region with one ruleset.

## Consequences

- Provider (game aggregator, PSP) credential storage must support both
  per-platform and per-tenant credential ownership, since which applies
  depends on `Tenant.licensing_model` and the specific vendor contract
  shape (Blueprint's own contract question: "does your integration permit
  sub-operators under our credentials?").
- Compliance rulesets (KYC/AML/RG/reporting) are resolved per
  `TenantJurisdictionConfig` row, not hardcoded per region — this must be
  true from the schema up, starting Stage 1's tenant/jurisdiction
  foundation tables.
- Geo-blocking and market permissions are audited, per-tenant,
  per-jurisdiction data — never a CDN-level rule.

## Owner

`architect` for the schema; `identity-compliance` for the compliance
rulesets it drives.
