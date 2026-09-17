# ADR 0037 — Asset/Currency Registry, FX Conversion Architecture, and Asset Authorization Boundary

Status: **NOT IMPLEMENTED — architecture only.** Recorded at the human's
direction (Stage 4H-B0-R4, `architect`), fulfilling the requirement
`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` §26
recommended and deferred, and closing that section's flagged **P1**: "the
asset-creation/activation authorization boundary is undefined" (§26.8).
This ADR designs; it does not build. No migration, admin API, provider
adapter, or FX implementation exists as a result of this document.

This ADR **does not**:
- expand the Bonus MVP (Stage 4H-B1's scope, gate, and rounding decision
  are unaffected — see "Relationship to ADR 0021" below);
- trigger FX implementation (no Conversion Service, no rate provider
  adapter, no migration is built here);
- add a payment or custody provider (no vendor is named or implied
  anywhere in this document);
- add retail implementation (ADR 0035 §9.4's multi-currency retail block
  is unaffected — see "Impact assessment");
- reopen or restate ADR 0021's rounding decision. DS-1 (round-half-up),
  DS-2 (round once, at the final monetary boundary, full `NUMERIC`
  precision until then), and DS-3 (one platform-wide rule, future
  per-asset/per-jurisdiction override left open) are **reused by
  reference only**. This ADR does not redefine, re-litigate, or restate
  their content.

## Context

Stage 4H-B0-R3 closed the Bonus Engine's financial gate and, while doing
so, found that the platform's `assets` table (migrations `0003`/`0006`)
is already storage-open — an unconstrained `TEXT` primary key, a
per-row `decimal_exponent` (0–18) that every consumer already looks up
rather than assumes, no code path anywhere in `internal/` that hardcodes
a decimal count. What is missing is the **operational surface**: an
admin API, an authorization model for who may create/activate an asset,
audit logging on that mutation, and the additional per-asset eligibility
concepts the product requirement names (deposit/withdrawal/wagering/
settlement/conversion/reporting) which today exist only indirectly and
only per-*provider*, through `ProviderCapability` — a different concept
serving a different question ("can this payment provider process this
asset for this tenant/brand," not "is this asset itself allowed to be
deposited at all").

Doc 27 §26 recommended this ADR by name (§26.2), scoped the FX/Conversion
boundary at a four-component level (§26.4), and flagged two P1s (§26.8):
the asset-creation/activation authorization boundary is undefined, and
fail-closed FX behavior should be written into this ADR as a binding
rule rather than improvised at implementation time. Both are closed
below (Part C and Part B's fail-closed section, respectively).

This ADR also reconciles with the existing precedent the platform already
has for tenant/jurisdiction currency availability —
`tenant_jurisdiction_configs.allowed_currencies` (migration `0002`) —
rather than inventing a parallel mechanism. See Part A, Layer 4.

---

## Part A — Extensible Asset/Currency Registry

### A.1 Principle

The platform's asset list is **open and extensible by design, not a
closed enum**. Existing fiat currencies, cryptocurrencies, network-
specific token variants, future assets not yet known, and internally-
defined/custom assets (e.g. a promotional point unit, a closed-loop
internal credit) are all representable as rows in the same registry, with
no maximum count and no schema change required to add one. This restates
and formalizes doc 27 §26.1's finding; it is not a new architectural
position.

**Adding a new asset must never require touching `internal/wallet` or
`internal/ledger` code.** A new row in the registry, plus registry-side
configuration (authorization/eligibility, defined below), must be
sufficient. Any code path found to branch on a specific asset code is a
`code-reviewer` blocking finding (this restates ADR 0007/0021's standing
rule, extended explicitly to the new authorization/eligibility surface
this ADR adds — that surface must be **data**, looked up, never a
compiled-in `switch` on asset code).

### A.2 The eight layers — kept explicitly distinct

The core design error this ADR exists to prevent is collapsing two of
these into one concept, or treating "the asset row exists" as if it
answered every downstream question. It does not. Each layer below is a
**separate gate**, independently owned, independently toggleable, and
independently queryable. An asset can be true at layer *N* and false at
layer *N+1* — that is not a bug, it is the point.

| # | Layer | Question it answers | Owner (which layer of the platform decides) | Storage (conceptual — not a migration) |
|---|---|---|---|---|
| 1 | **Asset existence** | Does a definition of this asset exist at all (code, type, exponent, display metadata)? | Platform Asset Registry | `assets` (existing table, extended — see A.4) |
| 2 | **Asset activation** | Is this asset row switched on at the platform level, at all, for anyone? | Platform Asset Registry (platform-admin only, see Part C) | `assets.active` (already exists) |
| 3 | **Platform authorization** | Is this asset usable on the platform in *any* operation, by *any* tenant, ever — a broader gate than raw activation, covering e.g. an asset that is defined and active for internal bookkeeping/testing but not yet cleared for any tenant-facing use | Platform Asset Registry (platform-admin only) | New: `assets.platform_authorized` (or equivalent) — see A.4 |
| 4 | **Tenant authorization** | Does *this* tenant offer this asset at all? | Tenant configuration, via jurisdiction | `tenant_jurisdiction_configs.allowed_currencies` (existing, migration `0002`) — reused, not replaced (see A.5) |
| 5 | **Brand authorization** | Does *this* brand (within an authorized tenant) offer this asset? | Brand-level configuration, narrowing tenant authorization | New, optional narrowing layer — see A.5; defaults to "same as tenant" when no brand-specific row exists, mirroring `ProviderCapability`'s existing nullable-`brand_id` "narrow from tenant default" pattern (ADR 0022 §3) |
| 6 | **Jurisdiction authorization** | Is this asset legal/permitted for players under this jurisdiction's ruleset? | Jurisdiction configuration | `tenant_jurisdiction_configs.allowed_currencies`, scoped per `(tenant_id, jurisdiction_id)` row (existing) — this is *the same mechanism* as Layer 4, not a separate one, because a tenant's jurisdiction config row is already the join point between "this tenant" and "this jurisdiction" (see A.5) |
| 7 | **Operation eligibility** | For an asset that has passed layers 1–6: is it eligible for *this specific operation* — deposit, withdrawal, wagering, settlement, conversion, reporting? | New: Asset Operation Eligibility (per-asset, optionally per-tenant-override) | New — see A.6 |
| 8 | **Market-rate availability** | Does a valid FX rate source exist for this asset right now? | FX/Conversion Service (Part B), *not* the Registry itself | The Registry only *exposes* whether a rate-source binding is configured (see A.7); whether the rate is *currently valid* is Part B's fail-closed check, evaluated at conversion time, never cached as a registry property |

**Explicit statement, restated because it is the whole point of this
section**: an asset being present in the registry (layer 1), or even
fully active and authorized at every layer through 6, does **not**
automatically make it depositable, withdrawable, wagerable, settleable,
or convertible. Layer 7 (operation eligibility) is an independent gate
that must be separately, explicitly granted per operation. A newly
listed cryptocurrency might be `active = true`, platform-authorized,
tenant-authorized, brand-authorized, and jurisdiction-authorized, and
still correctly have deposit eligibility `true` while withdrawal
eligibility remains `false` until an operational readiness bar is met
(e.g. custody withdrawal flow tested, AML travel-rule handling
confirmed) — this is not a workaround, it is the documented purpose of
splitting layer 7 out.

### A.3 Relationship between the layers — evaluation order

The layers are evaluated as an AND-chain, short-circuiting at the first
failure (cheapest/most platform-global checks first, consistent with how
`internal/rg` and `internal/risk` already order their own checks per ADR
0031 §1 — RG before Risk, cheapest and highest-authority gate first):

```
1 (exists?) → 2 (active?) → 3 (platform-authorized?) → 4 (tenant-authorized?)
  → 5 (brand-authorized?) → 6 (jurisdiction-authorized?) → 7 (operation-eligible?)
```

Layer 8 (market-rate availability) is **not** part of this chain — it is
evaluated only when the operation in question is itself a conversion,
and it is evaluated by the FX/Conversion Service at the moment of
conversion, never precomputed into the Registry's answer for layers 1–7
(a rate can go stale between two authorization checks; baking rate
freshness into an authorization row would let a stale cached "yes" leak
into a decision that must be re-verified every time — see Part B's
fail-closed rules).

### A.4 Extending `assets` — additive fields, no redesign of storage shape

`assets` (migrations `0003`/`0006`) already carries `code` (open `TEXT`
PK), `asset_type`, `decimal_exponent`, `display_name`, `active`,
`network`. This ADR adds, as **additive** columns/concepts (not a
migration — a future implementation stage's job):

- `platform_authorized` (boolean, layer 3) — distinct from `active`
  (layer 2). `active` is the platform-admin on/off switch for "does this
  row currently function at all" (e.g. temporarily disabling BTC
  platform-wide during a custody incident); `platform_authorized` is the
  slower-moving compliance/business gate for "has this asset cleared
  platform-level review to be offered to any tenant at all" (e.g. a new
  token that exists in the registry for internal testing but has not yet
  been cleared for any tenant-facing exposure). Collapsing these two
  would force an incident-response toggle and a compliance-clearance
  toggle to share one bit, which is exactly the kind of layer-collapse
  this ADR exists to prevent.
- `aliases`/`symbols` (optional, e.g. a display symbol distinct from the
  code, or a historical code an asset was renamed from) — metadata only,
  never used in ledger/wallet lookups, which remain keyed on `code`
  alone (ADR 0021's asset-identity decision is unchanged).
- Arbitrary `decimal_exponent` is **already supported** (0–18, `CHECK`
  constraint) — no change needed; this ADR does not touch the numeric
  representation decision (ADR 0021 §"Numeric representation").
- No new maximum-count constraint is introduced at any layer. "Do not
  invent an artificial maximum asset list" (directive) is satisfied by
  construction: nothing in layers 1–8 imposes a cardinality bound.

**Ownership, stated explicitly per the directive's request:**

| Concern | Owning layer/component |
|---|---|
| Asset identity (`code`) | Layer 1, Platform Asset Registry |
| Asset metadata (`display_name`, `asset_type`, `network`, aliases) | Layer 1, Platform Asset Registry |
| Exponent (`decimal_exponent`) | Layer 1, Platform Asset Registry — looked up, never hardcoded (ADR 0007/0021, unchanged) |
| Platform on/off status | Layer 2 (`active`), Platform Asset Registry |
| Platform-wide clearance to be tenant-facing at all | Layer 3 (`platform_authorized`), Platform Asset Registry |
| Tenant/brand/jurisdiction offering | Layers 4–6, tenant/jurisdiction/brand configuration (existing `tenant_jurisdiction_configs`, extended per A.5) |
| Per-operation eligibility | Layer 7, Asset Operation Eligibility (A.6) |
| Market-rate source binding (existence, not current validity) | Layer 8 exposure only; actual rate resolution is Part B's FX Rate Provider/Conversion Service |

### A.5 Reuse of `tenant_jurisdiction_configs.allowed_currencies` — extended, not replaced

Doc 27 §26.1 already found tenant/jurisdiction availability "already
solved" by `tenant_jurisdiction_configs.allowed_currencies` (migration
`0002`). This ADR confirms that finding and defines precisely how the
new layers relate to it:

- **Layers 4 and 6 (tenant and jurisdiction authorization) are the same
  underlying mechanism**, not two separate ones, because
  `tenant_jurisdiction_configs` is already keyed on
  `(tenant_id, jurisdiction_id)` — a row's presence and its
  `allowed_currencies` array jointly answer "is this asset offered by
  this tenant, under this jurisdiction's rules" in one place. This ADR
  does **not** introduce a second, parallel tenant-currency table; doing
  so would recreate exactly the "duplicated, potentially-conflicting"
  problem Part C exists to prevent, one layer earlier.
- **Layer 5 (brand authorization) is new** and does not yet have a home
  in any existing table. It is scoped as an *optional narrowing* of
  layer 4/6's tenant-level answer, following the same nullable-narrowing
  pattern `ProviderCapability` already uses for brand-specific payment
  routing (ADR 0022 §3: NULL `brand_id` = tenant-wide default, a brand-
  specific row narrows it for that brand). A future implementation would
  add a brand-scoped allow-list that, when absent for a given
  `(brand_id, asset_code)`, falls back to the tenant/jurisdiction answer
  computed from `tenant_jurisdiction_configs`; when present, it can only
  ever *narrow* (a brand can turn an asset off that its tenant/
  jurisdiction otherwise allows; it can never turn on an asset its
  tenant/jurisdiction has not authorized — narrowing is one-directional,
  the same non-negotiable invariant `ProviderCapability`'s own scoping
  already enforces implicitly by requiring the tenant relationship to
  exist first).
- `allowed_currencies` is `JSONB` today (an array of asset codes with no
  FK enforcement to `assets.code`). This ADR flags, as a
  non-blocking implementation-time item for whichever future stage
  builds this (explicitly **not** resolved here, consistent with this
  stage's architecture-only scope): a future migration should consider
  whether `allowed_currencies` remains a JSONB array or becomes a proper
  child table with an FK to `assets(code)`, so an asset cannot be
  "allowed" by a tenant config while not existing in, or while
  deactivated in, the registry. Either storage shape is compatible with
  this ADR's layering; the layering does not depend on which is chosen.

### A.6 Operation eligibility (layer 7) — new concept

Deposit, withdrawal, wagering, settlement, conversion, and reporting
eligibility are modeled as **one new, explicit concept**, not folded into
`active` (layer 2) or into `ProviderCapability` (which answers a
*provider's* capability, not the asset's own eligibility — an asset can
be withdrawal-eligible at the asset level while a *specific* payment
provider still cannot process it, and that remains `ProviderCapability`'s
job, unchanged and untouched by this ADR).

Conceptual shape (naming and exact storage left to the implementing
stage — this is architecture, not a migration):

```
AssetOperationEligibility
  asset_code       -- FK to assets.code
  operation        -- one of: deposit | withdrawal | wagering | settlement
                    --         | conversion | reporting
  eligible         -- boolean
  tenant_id        -- NULL = platform-wide default for this asset/operation;
                    -- non-NULL = tenant-scoped override, same nullable-
                    -- narrowing pattern as A.5/ProviderCapability
  reason_code      -- why (e.g. "pending AML travel-rule review",
                    -- "custody withdrawal path not yet certified")
  updated_by, updated_at   -- audit trail (mandatory, no exception — CLAUDE.md)
```

A platform-wide row (`tenant_id IS NULL`) sets the default; a
tenant-scoped row narrows it, following the exact same
narrow-only-never-widen rule as A.5's brand layer (a tenant may disable
an operation the platform defaults to enabled; it may never enable one
the platform has not itself enabled — this composes with, and does not
weaken, layer 3's platform authorization). `reporting` is included
because a jurisdiction's reporting format (CLAUDE.md's compliance
section) may require an asset to be reportable in a specific way before
it is allowed to carry real player value, independent of whether it can
be deposited or withdrawn yet.

### A.7 Market-rate source binding (layer 8) — Registry's exposure only

The Registry exposes, per asset, whether a rate-source binding is
*configured at all* (e.g. "BTC→EUR has a configured FX Rate Provider
binding"; "our internal loyalty-point asset has none, by design, because
it is never converted"). This is a static configuration fact the
Registry can answer instantly. Whether that binding currently returns a
*valid, fresh* rate is never a Registry fact — it is evaluated live, at
conversion time, by the Conversion Service (Part B). A custom/internal
asset with no market at all correctly has no binding configured, and any
attempt to convert it fails closed at Part B's first gate ("no valid
rate exists"), not because the Registry lied about eligibility, but
because layer 8 correctly reported "no source" and Part B refused to
proceed without one.

---

## Part B — FX / Asset Conversion Architecture

### B.1 Four components, kept structurally separate

Per doc 27 §26.4 and the human directive, four components are defined,
and this ADR keeps them structurally separate — no component's storage
or logic may be merged into another's:

1. **Asset/Currency Registry** (Part A) — what assets exist and their
   properties/authorization/eligibility.
2. **FX Rate Provider interface** — an external market-data source,
   behind a provider-neutral interface. No vendor is named.
3. **Conversion Service** — sits between the Rate Provider and the
   Ledger; applies a rate, applies ADR 0021's resolved rounding rule,
   produces a Conversion record (defined in B.4).
4. **The Ledger transaction** — ADR 0021's existing `ConversionOperation`,
   **not redefined here** (see B.5 for the precise relationship).

### B.2 FX Rate Provider interface — shape only, no vendor

```
FXRateProvider
  GetCurrentRate(ctx, sourceAsset, destinationAsset) (Rate, error)
      -- the provider's current best rate for the pair, as of "now"

  GetHistoricalRate(ctx, sourceAsset, destinationAsset, asOf time) (Rate, error)
      -- the provider's rate as of a past timestamp, if the provider
      -- supports historical lookup; returns a distinguishable
      -- "not supported" error if it does not (never a silently
      -- approximated rate)

  HealthCheck(ctx) (ProviderHealth, error)
      -- liveness/availability signal, used by provider selection (B.3)
      -- and by the fail-closed "rate source unavailable" gate (B.6)

Rate
  value            -- NUMERIC, never FLOAT/DOUBLE/REAL (ADR 0021's
                    -- "no floating point for money" rule extends to
                    -- rate values, restated, not reopened)
  precision        -- the number of significant/decimal digits the
                    -- provider asserts for this value
  as_of            -- the timestamp the rate is valid/quoted for
  provider_reference -- the provider's own identifier for this quote,
                    -- if it issues one (distinct from any internal
                    -- reference — see B.4)
```

This is an interface shape, not an implementation. No real vendor is
named or implied anywhere in this document, per the governance
requirement. A mock/sandbox implementation is explicitly left to the
future implementation stage doc 27 §26.3 already recorded as deferred.

### B.3 Provider selection and fallback — deterministic, defined precisely

"Deterministic" means: for a given `(source_asset, destination_asset)`
pair, the platform maintains an **ordered, configured priority list** of
bound `FXRateProvider`s for that pair (or a default ordering applied
when no pair-specific list is configured). Selection walks the list in
declared order and uses the **first provider that passes its own
`HealthCheck` and returns a valid rate under B.6's fail-closed
checks** — it never races multiple providers and takes whichever
responds first ("try things until one works" is explicitly excluded by
the directive, and is excluded here). The configured order is itself
versioned/stored data, not runtime-computed, so "why did this pair use
provider X" is always answerable by looking up the priority list as it
stood at `rate_timestamp`, not by reconstructing race timing after the
fact.

**The actual source used must be the one persisted — never the one
merely attempted first.** If provider A is first in priority but fails
its health check or returns an invalid rate, and provider B (second in
priority) supplies the rate actually used, the Conversion record's
`provider`/`source` field records B, not A. A record of which providers
were attempted and why each was rejected (health-check failure, stale
rate, malformed response) is an audit/observability concern the
implementing stage should log, but the field that feeds the ledger-
facing Conversion record names only the provider whose rate was
actually applied.

### B.4 The Conversion record

The Conversion record is what the FX/Conversion Service **produces**.
It is the layer above `ConversionOperation` (see B.5 for the precise
relationship) — it is the artifact of the FX-side computation before
that computation is translated into ledger postings.

```
Conversion
  conversion_id        -- this record's own identifier
  source_asset          -- FK to assets.code
  destination_asset      -- FK to assets.code
  source_amount          -- NUMERIC(38,0) minor units, source asset's exponent
  destination_amount     -- NUMERIC(38,0) minor units, destination asset's exponent
  rate                  -- NUMERIC, fixed declared scale (ADR 0021's
                          -- "never FLOAT/DOUBLE/REAL" rule, restated)
  rate_timestamp         -- the timestamp the applied rate is valid/quoted for
  base_asset             -- the base asset the rate was resolved through,
                          -- if the provider quotes via a base/reference
                          -- currency rather than the pair directly
                          -- (e.g. source→USD→destination); NULL when the
                          -- pair was quoted directly
  provider               -- which FX Rate Provider actually supplied the
                          -- rate used (B.3) — the platform's own
                          -- identifier for that provider binding
  provider_reference     -- the PROVIDER's own reference/quote ID for this
                          -- rate, if it issues one — distinct from any
                          -- platform-internal reference (see B.5's
                          -- reconciliation note; this is intentionally
                          -- the same concept ADR 0021's
                          -- `ConversionOperation.provider_reference`
                          -- names, carried through unchanged, not renamed)
  rate_precision         -- the precision/scale actually used for this
                          -- conversion's arithmetic
  rounding_rule_id        -- FK/version reference to ADR 0021's resolved
                          -- rounding decision (DS-1/DS-2/DS-3), the same
                          -- append-only `rounding_rules` reference table
                          -- ADR 0021 already specifies — not a second,
                          -- FX-specific rounding decision
  fee_amount             -- NUMERIC(38,0), same asset as fee_asset
  fee_asset              -- FK to assets.code
  spread                 -- NUMERIC, platform's spread if applied
  status                 -- e.g. quoted | applied | failed | expired
  created_at
```

**Fields added beyond doc 27 §26.4's list, and why**: `base_asset`
(needed because a provider may not quote every pair directly — a
triangulated rate through a configured base currency must record what
that base was, so the computation is reconstructable exactly, not just
approximately, per this ADR's "no live rate is the sole historical
source of truth" rule); `rate_precision` and `rounding_rule_id` (needed
so a stored Conversion is reconstructable against the *exact* precision
and rounding version applied, mirroring ADR 0021's own requirement that
`LedgerTransaction.rounding_rule_id` make the audit trail self-
sufficient even if the upstream computation row is later archived);
`status` (needed because not every Conversion the service produces
necessarily reaches the ledger — B.6 requires only `applied` may
proceed to `ConversionOperation`).

### B.5 Conversion record vs. `ConversionOperation` — the relationship, stated precisely

These are **not** the same fields under a different name, and they are
**not** duplicates of each other. They are two layers:

- The **Conversion record** (B.4) is what the FX/Conversion Service
  produces. It is the FX-side artifact: which provider, which rate, at
  what precision, under what rounding rule, before any ledger posting
  exists. It can exist in a `quoted` or `failed` state without ever
  becoming a ledger fact (e.g. a rate was fetched for a UI quote and the
  player never confirmed the conversion).
- `ConversionOperation` (ADR 0021, unchanged, not redefined here) is
  what actually gets **posted to the ledger** from an `applied`
  Conversion record. It is the ledger-facing terminus: it produces
  exactly one `LedgerTransaction` whose per-asset entries balance (ADR
  0021's two-leg clearing-account shape), and it is what the ledger's
  own idempotency/audit machinery governs.
- **Field correspondence**: `ConversionOperation.exchange_rate` /
  `.rate_source` / `.rate_timestamp` / `.fee_amount` /
  `.fee_asset_code` / `.spread` / `.provider_reference` are populated
  **from** the Conversion record's `rate` / `provider` / `rate_timestamp`
  / `fee_amount` / `fee_asset` / `spread` / `provider_reference` at the
  moment the Conversion Service hands off to the ledger. They are the
  same underlying facts, carried across the boundary — `Conversion` is
  the FX Service's own record of how it arrived at those facts (including
  fields the ledger has no reason to carry, like `rate_precision`,
  `base_asset`, and `status` for a quote that never got applied);
  `ConversionOperation` is the ledger's record of what it posted,
  scoped to exactly what the ledger-accounting invariants need. A
  `ConversionOperation` never exists without a corresponding `applied`
  Conversion record behind it; a Conversion record can exist without an
  accompanying `ConversionOperation` (any Conversion not in `applied`
  status). `conversion_id` (B.4) should be carried onto
  `ConversionOperation` as a foreign key (an additive field to ADR
  0021's schema, not a redefinition of it) so the ledger-side row can
  always be traced back to the exact FX Service record that produced
  it, and vice versa for reconciliation.

This directly answers the directive's question: the Conversion record
sits **one layer above** `ConversionOperation`. It is not a rename.

### B.6 Fail-closed rule — mandatory, explicit

A conversion **must fail closed** — never proceed, never substitute a
default/invented/zero rate, never silently downgrade precision — when
**any** of the following holds. This list is exhaustive for this ADR;
an implementing stage may add platform-specific checks but may not
narrow this list:

1. **No valid rate exists** for the requested pair (no provider binding
   configured at all — Registry layer 8 reports none — or every
   configured provider in priority order failed to return one).
2. **The rate is stale** beyond a configured freshness policy.
   "Stale" is defined precisely: `now() - rate.as_of > max_age`, where
   `max_age` is a configured, per-asset-pair (or platform-default)
   duration. **If no freshness policy is configured for a pair, the
   safe default is to treat the rate as stale** — absence of a policy
   is never read as "no limit"/"always fresh." This is the same
   fail-safe posture CLAUDE.md's Redis rule already takes for balances
   ("never authoritative, never read on the settlement path") — an
   unconfigured freshness bound must never be silently interpreted as
   unlimited freshness.
3. **The provider response is invalid or malformed** — missing a
   required field, a non-numeric or non-`NUMERIC`-representable rate
   value, a rate of zero or negative for an asset pair where that is not
   economically meaningful, or a `rate_timestamp` in the future.
4. **The asset is unauthorized for conversion** — layer 7 (operation
   eligibility, A.6) reports `conversion = false` for either the source
   or destination asset, at any applicable scope (platform, tenant, or
   brand narrowing). This is Part A's authorization boundary enforced at
   the FX boundary — the Conversion Service consumes the single
   canonical authorization check (Part C), it does not implement its own
   parallel eligibility logic.
5. **The asset is inactive** — layer 2 (`active = false`) for either
   asset.
6. **The rate source is unavailable** — the selected provider's
   `HealthCheck` fails, or times out, and no lower-priority provider in
   the configured list can supply a valid rate either.
7. **The rate's precision is insufficient for the requested operation**
   — e.g. a rate quoted at 2 significant digits being applied to convert
   an 18-decimal-exponent asset amount where that precision loss would
   itself introduce a materially wrong `destination_amount`. The
   Conversion Service must compare the rate's declared `precision`
   against the destination asset's `decimal_exponent` and refuse when
   the rate cannot support it, rather than silently computing with
   insufficient significant digits and rounding away the error.
8. **The conversion would violate a configured financial constraint** —
   this ADR does not invent what such a constraint is (e.g. a maximum
   single-conversion notional, a daily conversion cap); it only requires
   that the Conversion Service expose a hook a future constraint (most
   naturally expressed as an `internal/risk` rule, per ADR 0031's
   existing "one reusable engine" principle, rather than a second,
   FX-specific limits mechanism) can be evaluated against before a
   Conversion is allowed to reach `applied` status.

**No live market rate may be used without recording the exact rate
used.** Every financially material Conversion record that reaches
`applied` status must be reconstructable from persisted data alone — the
live provider response that produced it is never itself the historical
record; the Conversion record (B.4) is. A provider going away, changing
its API, or being decommissioned must never make a past conversion
unreconstructable.

Fail-closed behavior is a **hard refusal**, not a degraded-precision
best-effort — the Conversion Service returns a distinguishable error
(mirroring `internal/rg`/`internal/risk`'s existing pattern of specific,
distinguishable sentinel outcomes rather than a generic failure) and no
`ConversionOperation` is produced.

---

## Part C — Asset Authorization Boundary (resolves doc 27 §26.8's flagged P1)

### C.1 The problem this closes

Doc 27 §26.8 flagged: "`assets` is platform-wide with no RLS, so a
poorly-scoped 'authorized operator' grant could let one tenant's actor
add a row every tenant's ledger then references... `security`
recommends the eventual design evaluate a two-tier split (platform-
admin-only registration vs. tenant-scoped activation) before any
Registry API is built — the same shape of gap ADR 0031 §8 already
discloses for platform-wide risk rules, with larger blast radius here."

This ADR **confirms and refines** that two-tier split as the binding
authorization design, using the exact precedent already established and
implemented for the Risk & Limits Engine (ADR 0031): `risk_config:manage`
is held only by `RoleRiskManager`, which — like every non-`platform_
admin` `StaffRole` — is always tenant-scoped, so no tenant-scoped role
can create a genuinely platform-wide rule via the HTTP surface (ADR 0031
§8). The same shape applies here, with the same rationale, at a larger
blast radius (an asset row is referenced by every tenant's ledger, not
just one tenant's risk rules):

- **Layers 1–3 (existence, activation, platform authorization)** are
  **platform-admin-only** mutations. Only `RolePlatformAdmin` (or an
  equivalent future platform-scoped role — never a tenant-scoped role,
  by the same reasoning ADR 0031 §8 already applies to risk rules) may
  create an asset row, flip `active`, or flip `platform_authorized`.
  This is the correct place to draw the line doc 27 §26.8 asked for:
  registration and platform-level activation are **not** a tenant
  concern, and must never be reachable by a tenant-scoped role, full
  stop.
- **Layers 4–7 (tenant authorization, brand authorization, jurisdiction
  authorization, operation eligibility)** are **tenant-scoped**
  mutations, gated by a tenant-scoped permission (the natural home is
  alongside existing tenant-configuration permissions, not a new
  platform-wide one) — a tenant may only narrow within what platform
  layers 1–3 have already authorized; it can never widen past them
  (restated from A.5/A.6's narrow-only-never-widen rule, which is what
  makes this two-tier split safe rather than merely nominal).

### C.2 One canonical authorization concept — not five reimplementations

The critical design goal is avoiding what would otherwise happen:
wallet, payments, sportsbook, casino, bonus, FX, and retail each
independently querying `assets`/`tenant_jurisdiction_configs`/the new
eligibility tables and each getting the edge cases slightly wrong (a
classic drift risk, the same shape CLAUDE.md's provider-abstraction rule
and ADR 0031's "one reusable engine, never a separate limit engine per
product" already guard against for risk).

This ADR names and defines **one canonical service**, `AssetAuthorization`,
that every downstream domain consumes instead of reimplementing:

```go
func (a AssetAuthorization) CheckEligibility(
    ctx context.Context,
    tenant TenantID,
    brand BrandID,          // may be zero-value where an operation is not brand-scoped
    jurisdiction JurisdictionID,
    asset AssetCode,
    operation Operation,    // deposit | withdrawal | wagering | settlement
                            // | conversion | reporting
) (eligible bool, reason ReasonCode, err error)
```

`CheckEligibility` walks exactly the layer chain defined in A.3 (existence
→ active → platform-authorized → tenant-authorized → brand-authorized →
jurisdiction-authorized → operation-eligible), short-circuiting at the
first failing layer, and returns a **specific, distinguishable
`ReasonCode`** identifying which layer failed (mirroring `rg.Decision`
and `risk.RiskDecision`'s existing "specific, distinguishable sentinel,
never a generic denial" contract — the same interface shape convention
this platform already uses at its other two central decision points).
For a `conversion` operation specifically, `CheckEligibility` answers
layers 1–7 only; the FX/Conversion Service (Part B) separately evaluates
layer 8 and its own fail-closed rules (B.6) — `AssetAuthorization` is not
responsible for rate freshness or provider health, which are Part B's
concern, not an authorization concern.

**Every downstream domain — wallet, payments, sportsbook, casino, bonus,
FX/Conversion, retail — calls this one function** rather than querying
`assets`/`tenant_jurisdiction_configs`/the eligibility tables directly.
This is the same architectural posture as `internal/rg.EvaluateEligibility`
and `internal/risk.Evaluate`: a single canonical decision point, called
by every consumer, never reimplemented per-domain. `AssetAuthorization`
itself is the only component permitted to read the underlying
`assets`/`tenant_jurisdiction_configs`/eligibility tables for the
purpose of an eligibility decision — a domain reading those tables
directly to make its own allow/deny call, rather than calling
`AssetAuthorization`, is a `code-reviewer` blocking finding, the same
standing as a hardcoded per-asset branch (A.1) or a duplicated rounding
implementation (ADR 0021 item 4).

### C.3 Audit logging — mandatory, no exception

Every mutation at layers 1–7 (asset creation, activation flips at any
layer, eligibility changes) writes an audit record — actor, tenant (NULL
for a platform-scoped mutation), entity, before/after state, IP, reason
code — to the platform's existing append-only audit store (CLAUDE.md's
security section; no new audit mechanism is introduced). This restates,
for this domain, doc 27 §26.8's explicit "mandatory audit logging on this
mutation, no exception" finding, and closes it as a binding requirement
of this ADR rather than leaving it as an open risk note.

### C.4 Why this does not weaken isolation-tightening plans

`AssetAuthorization` reads platform-scoped (`assets`) and tenant-scoped
(`tenant_jurisdiction_configs`, eligibility tables) data from a single
service, which is consistent with — not in tension with — CLAUDE.md's
data-access-layer isolation-tightening path (shared cluster with RLS →
schema-per-tenant → database-per-tenant → cluster-per-tenant). The
service's own tenant-scoped reads still go through the same
connection-scoped tenant context every other tenant-scoped query uses;
centralizing the *decision logic* in one service does not centralize or
bypass the *data access* pattern each table already follows.

---

## Impact assessment

- **Bonus Stage 4H-B1**: none. This ADR is architecture-only and
  introduces no new blocking dependency on Bonus; the confirmation doc
  27 §26.7 already recorded (bonus computations operate on wallets
  already denominated in already-registered assets, independent of
  whether the Registry surface or FX boundary exist) is unchanged. ADR
  0021's rounding decision (DS-1/DS-2/DS-3) remains as recorded, reused
  by reference (B.4's `rounding_rule_id`), never reopened.
- **Retail (ADR 0035)**: no new blocker and no new unblock. ADR 0035
  §9.4's multi-currency retail block remains gated on the same
  conversion-clearing-account `OPEN DECISION` this ADR does not resolve
  (see below) — Registry/FX architecture existing on paper does not
  itself unblock cross-currency retail.
- **Conversion-clearing-account `OPEN DECISION` (ADR 0021 / `ledger-
  accounting-model.md` §2)**: this ADR does **not** resolve it. It
  remains a precondition for any `ConversionOperation` to actually post,
  independent of how well-designed the Registry/FX-provider boundary is.
  This ADR makes the dependency more visible, not less blocking.
- **Fee/spread revenue account `OPEN DECISION`** (ADR 0021's
  Consequences section): also not resolved here — out of this ADR's
  scope, a finance/business decision, not an engineering one.

## Governance

- Status: **NOT IMPLEMENTED.** No migration, admin API, RBAC permission,
  audit-log wiring, `AssetAuthorization` service, FX Rate Provider
  adapter, or Conversion Service exists as a result of this ADR. A
  future implementation stage (already recorded as deferred, doc 27
  §26.3) is required before any of Parts A–C become real.
- This ADR requires no change to ADR 0021's recorded rounding decision.
  DS-1/DS-2/DS-3 are reused by reference (B.4, B.5) and are not restated,
  re-litigated, or reopened anywhere in this document.
- Doc 27 §26 is the origin of this requirement. A pointer from doc 27
  §26 to this ADR (marking §26.2's "recommended future ADR 0037" as
  fulfilled) is **not made in this document** — doc 27 is not this ADR's
  file to substantially edit. **Flagged for the Orchestrator**: add a
  one-line pointer at the top of doc 27 §26 (e.g. "§26 is superseded/
  fulfilled by `docs/decisions/0037-asset-currency-registry-and-fx-
  conversion-architecture.md`") and update `financial-domain-model.md`'s
  `Asset` scoping-table row, which today says "See ... the recommended
  future ADR 0037" — that phrasing should become a direct link now that
  ADR 0037 exists, and its framing corrected from "Stage-1 seed set" to
  "open, extensible platform registry" per doc 27 §26.2's own recommended
  documentation change (also not made in this document, for the same
  reason — flagged for the Orchestrator).
- No real FX provider or vendor is named anywhere in this document.

## Open questions flagged for review (not resolved here)

1. **Storage shape of `allowed_currencies`** (A.5): JSONB array vs. a
   proper child table with an FK to `assets.code`. Left to the
   implementing stage; either is compatible with this ADR's layering.
2. **Exact RBAC permission name(s)** for layers 4–7's tenant-scoped
   mutations (C.1) — this ADR fixes the *shape* (tenant-scoped,
   narrow-only) but not the literal permission constant, which belongs
   with `internal/auth`'s existing permission table at implementation
   time, consistent with how `risk_config:manage` was named for ADR 0031.
3. **Financial-constraint hook** (B.6 item 8) — this ADR requires the
   hook exist, not what any specific constraint value should be; left to
   `risk`/`ledger-finance` at implementation time.
4. **Brand-layer default table shape** (A.5, layer 5) — this ADR fixes
   the narrow-only-from-tenant semantics; the concrete table/column
   design is implementation-stage work.

These are handed to `ledger-finance` (financial correctness of Part B),
`security` (Part C's authorization/audit design), and `qa`
(extensibility — confirming no new code path anywhere hardcodes an
asset, an operation list, or a layer count) for the second-wave review
this stage's directive specifies.

## Owner

`architect` (cross-domain boundary and layering design), `ledger-finance`
(Part B's financial correctness once implementation begins), `security`
(Part C's authorization boundary).
