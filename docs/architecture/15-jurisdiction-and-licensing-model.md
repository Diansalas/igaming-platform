# 15 — Jurisdiction and Licensing Model (Hybrid)

Status: Accepted. Source: human-approved Stage 0 business decision
(`docs/decisions/0006-hybrid-licensing-and-jurisdiction-model.md`),
superseding the Stage 0 draft assumption that all tenants sat under one
licensing posture.

## Decision recap

The platform must support, simultaneously:

- **A. Platform-licensed tenants** — brands operating under our own
  gambling licence (starting with Anjouan).
- **B. Self-licensed tenants** — brands operating under their own
  licence, in their own jurisdiction, with their own regulatory
  obligations.

Licensing is therefore tenant-level configuration, never a global
platform assumption, and never a code path.

## Data model

```
Jurisdiction
  id, code (e.g. "KM-ANJ", "MT", "CO"), name
  regulatory_body, notes

Licence
  id, jurisdiction_id -> Jurisdiction
  licensee ("platform" | "tenant"), licence_number, status, issued_at,
  expires_at, permitted_products (jsonb: casino/sportsbook/etc.),
  permitted_markets (jsonb: list of jurisdiction codes allowed to be served)

Tenant
  id, name, licensing_model ("under_platform_licence" | "own_licence")
  licence_id -> Licence (the licence this tenant actually operates under —
    either our platform licence, shared across platform-licensed tenants,
    or a licence the tenant brought)

TenantJurisdictionConfig
  tenant_id -> Tenant, jurisdiction_id -> Jurisdiction
  kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, reporting_ruleset_id,
  allowed_currencies (jsonb), allowed_payment_methods (jsonb),
  geo_block_list (jsonb), effective_from, effective_to
```

A tenant has exactly one `licensing_model` and one operative `Licence`.
A tenant can, however, serve **players from multiple jurisdictions**, each
governed by its own `TenantJurisdictionConfig` row — this is what makes
Europe + LATAM (Blueprint decision, see `0006`) representable without
hardcoding either region's rules into business logic.

## Enforcement points

- **Registration / geo-gating**: player's detected jurisdiction is
  resolved, checked against the tenant's `TenantJurisdictionConfig` for a
  matching, active row; no row or an inactive row blocks registration/play
  and is logged (never a silent CDN-level rule).
- **KYC/AML/RG**: `identity-compliance` resolves the applicable ruleset
  IDs from `TenantJurisdictionConfig` at runtime — rules themselves live
  in the compliance subsystem (`11-kyc-aml-rg-architecture.md`), keyed by
  ruleset id, not hardcoded per region.
- **Payments**: `payment-orchestrator` filters available methods/
  currencies by the player's `TenantJurisdictionConfig` row before
  routing (`07-payments-architecture.md`).
- **Reporting**: `data-analytics` selects the export format by
  `reporting_ruleset_id` (`12-audit-reporting-architecture.md`).
- **Provider availability**: game/sportsbook catalogue filtering already
  has a jurisdiction blocklist concept (Blueprint §4.3); it now resolves
  against `TenantJurisdictionConfig` rather than a single global list.

## What this explicitly rules out

- A single `jurisdiction` enum hardcoded into business logic.
- Assuming "Europe" or "LATAM" is one ruleset — each country/regulator is
  its own `Jurisdiction` row with its own config.
- Baking Anjouan-specific market-blocking logic into code — it is one row
  in `TenantJurisdictionConfig` for platform-licensed tenants, replaceable
  per tenant.

## Known gap (tracked, not yet enforced)

Nothing in the Stage 1 schema ties `tenants.licensing_model` to
`licences.licensee` — a tenant marked `own_licence` could technically be
pointed at a licence row marked `licensee = 'platform'`, or vice versa.
Enforcing this cross-table invariant needs either a trigger or
application-level validation; deferred to the Stage 2 tenant-config
service (where tenant provisioning actually happens) rather than added as
a database trigger now, to avoid encoding business logic in SQL before
the owning service exists. Flagged in Stage 1 specialist review so it
isn't silently forgotten.

## Stage mapping

Stage 1 establishes the `tenants`, `jurisdictions`, and licensing schema
skeleton plus the tenant-context/RLS foundation these tables rely on.
Full `TenantJurisdictionConfig`-driven enforcement (geo-gating, KYC/AML/RG
ruleset resolution, payment filtering) is implemented as each owning
subsystem is built (Stages 2–4).

## Ownership

`architect` owns this schema; `identity-compliance` owns the KYC/AML/RG
ruleset content it points to; `payments` and `data-analytics` consume it
for their own filtering.
