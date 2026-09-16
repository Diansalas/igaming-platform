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

## Licensing-model/licence consistency (closed in Stage 2)

The Stage 1 gap noted here — nothing tied `tenants.licensing_model` to
`licences.licensee`, so a tenant marked `own_licence` could technically
point at a licence row marked `licensee = 'platform'`, or vice versa — is
now enforced at the database layer, not just in application code.
Migration `0007_tenant_slug_and_licence_consistency` adds a
`UNIQUE (id, licensee)` constraint on `licences`, a generated
`tenants.expected_licensee` column (`'platform'` when
`licensing_model = 'under_platform_licence'`, else `'tenant'`), and a
composite foreign key
`tenants (licence_id, expected_licensee) REFERENCES licences (id, licensee)`.
A contradictory state is now a constraint violation, not just a bug some
future service could introduce — see migration
`0007_tenant_slug_and_licence_consistency`'s own comments (there is no
dedicated ADR for this fix), migration `0017_tenant_licence_consistency_
guard` (a defense-in-depth `CHECK` closing a theoretical silent-bypass
edge case flagged in Stage 2 architect review — see that migration's
comments for why the FK alone wasn't quite enough), and the Stage 2
completion report for the verifying test
(`TestTenant_LicensingModelMustMatchLicenceLicensee`).

## Stage mapping

Stage 1 establishes the `tenants`, `jurisdictions`, and licensing schema
skeleton plus the tenant-context/RLS foundation these tables rely on.
Stage 2 closes the licensing-model/licence consistency gap (above), adds
the `Brand` concept as distinct from `Tenant`
(`docs/decisions/0012-brand-distinct-from-tenant.md`), and exposes tenant/
brand provisioning via the admin API — `TenantJurisdictionConfig` itself
is untouched structurally in Stage 2 (still Stage 1's schema; no new
jurisdiction rows or ruleset content added). Full
`TenantJurisdictionConfig`-driven enforcement (geo-gating, KYC/AML/RG
ruleset resolution, payment filtering) is implemented as each owning
subsystem is built (Stages 3–4).

## Risk & Limits consumption (Stage 4G-FINAL)

`internal/risk` (Stage 4G, `docs/decisions/0031-risk-and-limits-engine.md`)
consumes two fields from this model directly, as plain scope dimensions
on `RiskRequest`/`Rule` - it never queries `jurisdictions`, `licences`, or
`tenants` itself, and every value below is resolved by the CALLING domain
from its own trusted context, exactly like `TenantID`/`BrandID` already
are:

- **`JurisdictionCode`** (`jurisdictions.code`) - lets a risk rule express
  a legal/jurisdiction-specific ceiling (e.g. a `HARD_LIMIT` on max stake
  for a specific market). Migration 0042 added
  `casino_launch_sessions.jurisdiction_code`, populated once at launch
  time from whatever jurisdiction context `LaunchGame`'s own caller
  resolved, and read back by `postBet` for every subsequent bet in that
  round - see ADR 0031 §9 for the full design and why this was previously
  reachable only at launch time, not bet time.
- **`LicensingMode`** (`tenants.licensing_model`) - lets a risk rule
  distinguish "this is our OWN platform licence's legal ceiling" from "a
  general commercial policy," so a platform-wide `HARD_LIMIT` does not
  accidentally bind a future self-licensed (BYOL) tenant operating under
  a completely different licence's own legal regime. See ADR 0031 §10.
  No BYOL tenant is onboarded by this addition - it is the contract a
  future one will rely on.

Both remain OPTIONAL scope dimensions (empty/unscoped matches every
value of that dimension) - existing and future rules that don't need
this distinction are entirely unaffected. Full precedence/specificity
rules for how these interact with every other `Rule` dimension are
`internal/risk`'s own concern (ADR 0031 §5), not duplicated here.

## Ownership

`architect` owns this schema; `identity-compliance` owns the KYC/AML/RG
ruleset content it points to; `payments` and `data-analytics` consume it
for their own filtering; `risk` consumes `jurisdiction_code`/
`licensing_model` as read-only `RiskRequest` scope dimensions (Stage
4G-FINAL) without owning or modifying this schema.
