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

> **IMPLEMENTATION STATUS (corrected by `architect`, Stage 4I — this
> corrects RECON finding C-7 and discharges
> `docs/governance/stage-4i-canonical-model.md` §12.3 item 3).**
>
> **The five enforcement points listed below are the intended design.
> `NOT IMPLEMENTED` — zero of the five are wired at this commit.** They
> are retained as the design statement, not as a description of platform
> behaviour. Specifically, as of Stage 4I:
>
> - **Registration / geo-gating** — there is no producer of a "player's
>   detected jurisdiction" anywhere in the platform. Collecting any
>   player residence/location/nationality attribute is an unanswered
>   human decision (**HDR-J-3**,
>   `docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md`),
>   so every player-scoped jurisdiction resolution returns
>   `unresolved(no_signal)` (canonical-model §11.3). No registration or
>   play path consults `TenantJurisdictionConfig`.
> - **KYC/AML/RG** — `internal/kyc` and `internal/rg` do not resolve
>   ruleset ids from `TenantJurisdictionConfig`. RG's own
>   jurisdiction dimension (self-exclusion policy, migration `0043`) is
>   keyed on `jurisdictions` directly, not through this table.
> - **Payments** — `internal/payments`' routing dimension 2 is an
>   explicit `TODO(jurisdiction)`. `AdapterCapability.SupportedCountries`
>   is an ISO-3166 **payment-rail capability** fact and is **not** a
>   jurisdiction gate, is not in `jurisdictions.code` space, and must
>   never be substituted for one without an explicit, stored, audited,
>   `security`-reviewed mapping — see
>   `docs/governance/stage-4i-payments-model.md`.
> - **Reporting** — no export path reads `reporting_ruleset_id`.
> - **Provider availability** — `casino_games.jurisdiction_blocklist`
>   **is** now genuinely enforced (Stage 4I item K-3,
>   `internal/casino`'s `LaunchGame`), but it resolves against the game's
>   own blocklist array and a server-resolved `jurisdiction.Resolution`,
>   **not** against `TenantJurisdictionConfig`. The control is armed per
>   game with a non-empty blocklist and fails closed within it
>   (canonical-model §9.2).
>
> **`allowed_currencies` is superseded for the asset-availability
> question** by `asset_authorizations` (migration `0045`, whose own header
> says so) and must not be read for that purpose. It is not dropped —
> dropping a column on an RLS'd, effective-dated table is its own change
> with its own review — and nothing reads it today either way
> (canonical-model §1.3, RULING BI-4I-3).
>
> **A brand has no independent operating jurisdiction** (canonical-model
> §1.1, RULING BI-4I-1): brand is a *narrowing* dimension over the
> tenant's jurisdiction set, never an independent source of one. This is
> why `TenantJurisdictionConfig` correctly carries no brand dimension
> while `asset_authorizations` / `risk_rules` /
> `open_bet_self_exclusion_policies` carry brand alongside jurisdiction.
>
> **What jurisdiction resolution actually is, as of Stage 4I**:
> `internal/jurisdiction` (the resolver) plus `jurisdiction_resolutions`
> (the append-only record) plus the `jurisdictions`/`licences` registry
> write surface. `docs/governance/stage-4i-canonical-model.md` is the
> canonical and binding contract; this document remains the schema/model
> statement it is implemented against.

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

**Correction (Stage 4I Phase E-SECURITY, migration 0077, ADR 0046):**
"A contradictory state is now a constraint violation, not just a bug some
future service could introduce" overstated the composite FK's coverage.
The FK's own `expected_licensee` is a `GENERATED` column recomputed from
the row's CURRENT `licensing_model` at constraint-check time — a single
`UPDATE tenants SET licensing_model = ..., licence_id = ... WHERE id =
...` that changes BOTH columns together never produces a self-
contradictory (old-model, new-licence) pair for the FK to reject; it only
ever validates a self-consistent NEW state. This was live-reproduced
(architect, ADR 0046) as an ordinary tenant-scoped write, prior to
migration 0077. The FK still correctly rejects the SINGLE-column case
(changing only `licence_id`, or only `licensing_model`, while the other
is held fixed) and is not itself defective - the real gap was the
complete absence of row-level security on `tenants`, which meant nothing
prevented a tenant-scoped connection from issuing the combined UPDATE in
the first place. Migration 0077 closes this by removing tenant-scoped
write access to `tenants` entirely, not by changing the FK.

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

## Operating market / country policy (Stage 4I Phase E) — a SEPARATE model

**This is a different question from everything above.** This document's
`Jurisdiction`/`Licence`/`TenantJurisdictionConfig` model, and
`internal/jurisdiction`'s resolver, answer "which REGULATORY jurisdiction
governs this player/tenant, and on what basis". Stage 4I Phase E
(`docs/decisions/0045-operating-market-and-country-policy-foundation.md`)
answers a structurally different question: "for a tenant/brand, an
operation (registration/deposit/withdrawal/wagering), optionally a
product (casino/sportsbook/...), a COUNTRY — is this platform actually
permitted to operate, given both the licence's ceiling and every narrower
policy decision beneath it?"

The two are kept separate at the **package boundary**, not by convention:
the mechanism lives in a new package, `internal/operatingmarket`, which
may import `internal/jurisdiction` for exactly one function
(`EvaluateLicenceValidity`, the shared, single technical implementation
of "is this licence currently reliable") and nothing else — it cannot
import `internal/identity`/`internal/kyc`/`internal/geolocation`/
`internal/rg`, and it structurally cannot reach `PlayerJurisdictionResult`,
`EvidenceSet`, or `Resolution`. `internal/jurisdiction` itself has **zero
diff** from Phase E, aside from one pure addition
(`internal/jurisdiction/licence_validity.go`) and administrative-metadata
fields on the existing `jurisdictions`/`licences` write surface (below).

Three new tables (migration `0076`), all **country-code keyed
(ISO-3166-1 alpha-2)**, deliberately never joined or compared to
`jurisdictions.code` (a different code space — `KM-ANJ` is sub-national;
`MT`/`CO` match ISO alpha-2 only coincidentally, per
`internal/validation/country.go`'s own governing rule):

- **`platform_operations`** — an extensible OPERATION vocabulary
  (`registration`/`deposit`/`withdrawal`/`wagering`), deliberately
  disjoint from `jurisdiction.OperationClass` (the player-jurisdiction
  resolver's own 4-value call-site taxonomy), `asset_operation_eligibility.
  operation`, and `risk_rules.operation`. The PRODUCT dimension reuses
  `platform_products` unchanged — product and operation are two
  independent dimensions, not one flat enum.
- **`licence_country_ceilings`** — the platform-wide, append-only,
  effective-dated ceiling a LICENCE places on which countries may ever be
  enabled beneath it. Authoritative and sole source of a licence's
  permitted countries; `licences.permitted_markets` (this document's own
  schema block above) is now **DEPRECATED and non-authoritative** — see
  that column's own `COMMENT` and task-registry item `MKT-PM-1`.
- **`operating_country_policies`** — the tenant/brand/operation-scoped,
  append-only statement of whether a tenant actually operates in a
  country, narrowing (never exceeding) the licence ceiling above.

`jurisdictions` itself gained one administrative-metadata column,
`country_code` (nullable, not unique, never auto-assigned by the
migration) — the ISO-3166-1 alpha-2 country a regulatory jurisdiction
sits inside, where unambiguous. **This is fenced, mechanically tested
metadata, not a resolver**: it must never be used to derive a player's
jurisdiction from a residence/location country, and no code anywhere in
`internal/jurisdiction`'s player-resolution path may reference it
(`TestJurisdictionCountryCode_IsNotAJurisdictionResolver`).

Resolution (`internal/operatingmarket.ResolveOperatingCountryPolicy`) is
a pure function of `AsOf` and the current row set — never cached, never
persisted as a `*_resolutions` table (unlike `jurisdiction_resolutions`,
which records a player-affecting determination regulators require be
reconstructible; an operating-market answer's own inputs already are
reconstructible, so storing the answer would be a second, driftable
copy). It has **zero production callers** as of Phase E: no HTTP route,
no OpenAPI change, and no consuming domain (`casino`/`bonus`/`risk`/
`payments`/`sportsbook`/registration/withdrawal) has been wired to call
it. Full design, the five-step algorithm, the eleven-outcome result type,
and the permission model are ADR 0045's own subject matter, including
ADR 0045 §3.5-A AMENDMENT-1's correction of the operation rung to a SET
evaluated with first-disabled-wins (not most-specific-wins), AMENDMENT-2's
CHECK requiring authorization on an inherit-rung withdrawal (§17), and
AMENDMENT-3's deferred constraint trigger requiring an authorized
successor on an inherit-rung close (§18) — plus finding F4's correction of
`EvaluateLicenceValidity` to a half-open `issued_at`/`expires_at`
interval. ADR 0045 remains authoritative for all of the above; this
document is not updated further as amendments land.

## Ownership

`architect` owns this schema; `identity-compliance` owns the KYC/AML/RG
ruleset content it points to; `payments` and `data-analytics` consume it
for their own filtering; `risk` consumes `jurisdiction_code`/
`licensing_model` as read-only `RiskRequest` scope dimensions (Stage
4G-FINAL) without owning or modifying this schema.

**Jurisdiction *resolution*** — as distinct from this schema — is owned
separately, per `docs/governance/stage-4i-canonical-model.md` §2.1 and
the corresponding row in `docs/governance/ownership.md`: the interface
contract is `architect`'s, the implementation (`internal/jurisdiction`)
is `backend`'s, and the source-precedence ruleset **content** (once
HDR-J-2 is answered) is `identity-compliance`'s — mirroring the
schema/content split this document already uses for KYC/AML/RG rulesets.
