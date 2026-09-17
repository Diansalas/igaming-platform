# ADR 0037 — Asset/Currency Registry, FX Conversion Architecture, and Asset Authorization Boundary

Status: **PARTIALLY IMPLEMENTED.** Parts A (layers 1-7) and C are
IMPLEMENTED as of Stage 4H-B0-R6, Workstream A — see **§C.6** for the
implementation record, the four amendments it makes to this document's
own earlier text, and the explicit list of what remains undone. Part B
(FX/Conversion) and layer 8 remain **NOT IMPLEMENTED**: no FX provider,
Conversion Service, rate binding or plausibility bound exists. Everything
below §C.6 predates implementation and is read as the design record;
where §C.6 amends it, §C.6 governs.

Originally recorded as **NOT IMPLEMENTED — architecture only** at the human's
direction (Stage 4H-B0-R4, `architect`), fulfilling the requirement
`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` §26
recommended and deferred, and closing that section's flagged **P1**: "the
asset-creation/activation authorization boundary is undefined" (§26.8).
This ADR designs; it does not build. No migration, admin API, provider
adapter, or FX implementation exists as a result of this document.
**Extended at Stage 4H-B0-R5** (`architect`) by B.7 (rate-plausibility
contract) and C.5 (administrative API surface), closing that stage's P1-1
and P1-2; both additions are architecture-only, like everything else in
this document.

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
  `.fee_asset_code` / `.spread` / `.provider_reference` /
  `.rounding_rule_id` are populated **from** the Conversion record's
  `rate` / `provider` / `rate_timestamp` / `fee_amount` / `fee_asset` /
  `spread` / `provider_reference` / `rounding_rule_id` at the moment the
  Conversion Service hands off to the ledger. **`rounding_rule_id` crosses
  this boundary too** (Stage 4H-B0-R4 Wave-2 review correction,
  `ledger-finance`) — omitting it here, despite B.4 already justifying
  its presence on the Conversion record by citing ADR 0021's own
  requirement that the identifier reach the ledger row, would silently
  violate that same requirement for every FX conversion. **The request
  context `ConversionOperation` also requires — `tenant_id`,
  `player_account_id`, `source_wallet_id`, `destination_wallet_id`,
  `idempotency_key` — is supplied by the calling wallet/ledger layer that
  invoked the Conversion Service, not by the Conversion record itself**
  (Stage 4H-B0-R4 Wave-2 review correction, `ledger-finance`): the FX
  Service is asset-pair-scoped, not wallet-scoped, so the Conversion
  record carries no tenant/player/wallet identity of its own, and is not
  itself an RLS-isolated, tenant-scoped table — it is FX-service-internal
  bookkeeping keyed by `conversion_id` alone, joined to its eventual
  `ConversionOperation` (which IS tenant-scoped, under existing ledger
  RLS) via the `conversion_id` FK described below. They are the
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

**These 8 checks establish structural fail-closed behavior — they do
not, by themselves, detect a well-formed rate that is economically
implausible or the product of a compromised or malicious provider**
(Stage 4H-B0-R4 Wave-2 review correction, `security` and `ledger-finance`
independently). A provider returning a rate that is valid, fresh,
well-formed, for an authorized+active asset pair, from a healthy source,
at sufficient precision, and within whatever financial-constraint hook
exists, can still be wrong by an arbitrary factor or a subtle,
exploitable skew — none of items 1–8 validate the rate's *magnitude*
against a plausibility bound. A rate-plausibility check (e.g. a maximum-
deviation bound evaluated against `GetHistoricalRate`, B.2, before a
Conversion is allowed to reach `applied` status) is a **required
implementation-time control before any live FX provider is connected —
not optional hardening, and not resolved by this ADR.** This must be
stated explicitly rather than left for a future implementer to
(wrongly) infer that the 8 structural checks are a complete defense
against an adversarial or malfunctioning rate source.

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

### B.7 Rate plausibility — resolving the Stage 4H-B0-R4 deferred item (Stage 4H-B0-R5, `architect`)

B.6's eight conditions (unchanged, referenced here, not restated) are
exhaustive for **structural** fail-closed behavior. The Wave-2 correction
appended to B.6 named the remaining gap precisely: none of the eight
validate a well-formed rate's **economic plausibility**, and named a
rate-deviation-bound check as a required implementation-time control, not
resolved by that ADR. This section resolves it, at the architecture level,
without touching B.6's text or numbering.

**B.7.1 — Classifying every named failure mode.** The ten failure modes
this stage's directive names are classified below into **A — universal
financial validity checks** (apply to every conversion, every pair,
unconditionally) and **B — configurable market-specific plausibility
checks** (need per-asset-pair or platform-default configuration to
evaluate). This is not a re-derivation of B.6 — eight of the ten are
already-resolved structural checks, restated here only so the two-kind
split is explicit and so the two genuinely new checks are visibly the only
additions.

| Failure mode | Kind | Resolution |
|---|---|---|
| Malformed rate (missing field, non-`NUMERIC` value) | **A** | B.6 item 3 — already resolved, unchanged. |
| Zero rate (where not economically meaningful) | **A** | B.6 item 3 — already resolved, unchanged. |
| Negative rate | **A** | B.6 item 3 — already resolved, unchanged. |
| Missing timestamp (`as_of` absent) | **A** | B.6 item 3 ("missing a required field"). A `Rate` with no `as_of` cannot even be evaluated for staleness (item 2), so this is refused before staleness is checked — the two checks compose, neither substitutes for the other. |
| Missing provider reference (`provider_reference` absent) | **A, with one precision — resolved here** | B.2 already states `provider_reference` is populated "if [the provider] issues one," so bare absence is not automatically a failure. **Resolution**: an `FXRateProvider` binding declares, as a one-time capability fact (not a per-pair configuration), whether it issues `provider_reference` values at all. If it declares that it does and a quote omits one, that quote is malformed under B.6 item 3 (fail closed). If it declares that it does not, absence is expected. This stays category **A** — it depends on a fixed per-provider-binding fact, not a market judgment — so it does not become a category-B tolerance. |
| Future timestamp (`as_of` after `now()`) | **A** | B.6 item 3 — already resolved, unchanged. |
| Stale rate | **A**, universal rule / configurable threshold | B.6 item 2 — already resolved, unchanged. The requirement to fail closed on staleness (including "no configured policy means stale") is universal; only the numeric `max_age` is per-pair/platform-default data. |
| Invalid/insufficient precision | **A**, universal rule / Registry-derived bound | B.6 item 7 — already resolved, unchanged. The bound is the destination asset's own `decimal_exponent` (a Registry fact, Part A), not a business/risk judgment, so it is not category B. |
| Rate source unavailable | **A** | B.6 item 6 — already resolved, unchanged. |
| **Implausible magnitude** (well-formed, fresh, authorized, from a healthy source, but wrong by an arbitrary or subtly exploitable factor) | **B — new** | Resolved in B.7.2. |
| **Provider disagreement** (two configured, healthy providers for the same pair return meaningfully different current rates) | **B — new** | Resolved in B.7.3, as new fail-closed condition 9. |

**B.7.2 — Magnitude plausibility (category B).** For a given
`(source_asset, destination_asset)` pair, the Conversion Service evaluates
the selected provider's `GetCurrentRate` result against that **same
provider's own** `GetHistoricalRate` for a recent, configured lookback
window (the window length is configurable per pair or platform-default —
this ADR does not fix it, the same way B.6 item 2 does not fix `max_age`).
If the proportional deviation between the current rate and that historical
baseline exceeds a configured **maximum deviation bound** (per-pair, or a
platform default when no pair-specific bound is configured), the rate is
implausible and the conversion fails closed.

- **Fail-closed default for absent configuration**, restated a third time
  for consistency (B.6 item 2, C.1, now here): if no deviation bound is
  configured for a pair and no platform default exists, the check cannot
  be evaluated, and an unevaluable check fails closed — absence of a bound
  is never read as "no limit."
- If the selected provider does not support `GetHistoricalRate` for this
  pair (B.2 already permits a distinguishable "not supported" response
  rather than an approximated one), the check cannot run for that
  provider. This is **not** "check passed" — a provider that cannot supply
  its own historical baseline cannot have its current quote's plausibility
  verified, and the conversion fails closed on the same "absence is never
  read as permission" principle.
- This check is per-provider and single-source: it asks only "does this
  provider's own current rate look like a plausible continuation of this
  provider's own recent history," independent of whether any other
  provider is configured. It therefore runs even when only one provider is
  configured for a pair.

**B.7.3 — Provider disagreement (category B) — new fail-closed condition
9.** This resolves the case B.3's existing "ordered priority list, first
healthy wins" design does not address: a second, lower-priority,
independently configured, healthy provider bound to the same pair returns
a **meaningfully different** rate from the priority provider's, at the
same evaluation time.

**Decision: priority order alone is not sufficient here. Material
cross-provider disagreement is itself a new fail-closed trigger —
condition 9 — not a tolerated, silently-resolved-by-priority outcome.**

Rationale: B.3's priority list answers a *selection* question ("which one
rate do we use") on the assumption that a lower-priority provider exists
as a fallback for *unavailability*. It was never designed to, and does
not, catch the case where both providers are reachable, healthy, and each
independently passes B.7.2's own magnitude check against its *own*
history — and still disagree with each other by more than noise. That
combination is exactly what a compromised, mis-configured, or
malfunctioning provider produces when its own historical baseline has
*also* drifted (so B.7.2 alone cannot catch it) — and it is the same
posture CLAUDE.md's ledger-reconciliation rule already takes elsewhere
("any non-zero drift is a P1 incident"): an unexplained mismatch between
two things that should agree is never silently accepted. Extending that
posture to FX rates is a direct application of an existing platform
principle, not a new one invented here.

**Mechanism, using `GetHistoricalRate` (B.2) as the mechanism, per the
task's own naming of it:**

1. This check runs only when **two or more configured providers for the
   same pair are simultaneously healthy**, and each has **already,
   independently, passed B.7.2** (its own current-vs-its-own-history
   check). If only one provider is healthy/available, there is nothing to
   disagree with — condition 9 does not apply, and B.3's selection is
   unmodified.
2. Requiring each side to pass its own historical-deviation check first
   matters: it prevents one known-bad provider from being used to falsely
   "prove" a good provider wrong, or vice versa — disagreement is only
   evaluated between two providers that each already look internally
   plausible on their own.
3. The Conversion Service computes the spread between the priority
   provider's current rate and the next-priority healthy provider's
   current rate. If that spread exceeds a configured **maximum acceptable
   spread** (per-pair or platform default; same "absent configuration
   fails closed" rule as B.7.2), the conversion fails closed under
   condition 9.
4. On condition 9, the Conversion Service does **not** fall through to a
   third provider or otherwise route around the disagreement — a
   disagreement between two independently-plausible sources is evidence
   about the *pair's rate environment*, not evidence a third source would
   be more trustworthy. It is a hard refusal (the same distinguishable-
   error contract as every B.6 condition), and it additionally raises an
   operational alert to a human, mirroring the reconciliation-drift P1
   posture — this is new: none of B.6's original eight require a human
   alert, only a refusal. Condition 9 requires both, because a
   disagreement between two independently-plausible, healthy, configured
   sources is worth a human's attention even after the conversion has
   already been correctly refused.

**Condition 9, stated in B.6's own list format, for direct incorporation
by an implementing stage** (additive — B.6's exhaustiveness statement is
amended to admit exactly this one new condition, per this ADR's own
governance rule that "an implementing stage may add platform-specific
checks but may not narrow this list"):

> 9. **Configured-provider material disagreement** — two or more
>    configured `FXRateProvider` bindings for the same pair are
>    simultaneously healthy, each independently passes B.7.2's magnitude-
>    plausibility check, and their current rates diverge beyond a
>    configured maximum-acceptable-spread bound (or no bound is
>    configured, which fails closed identically). The conversion is
>    refused and an operational alert is raised; the Conversion Service
>    does not proceed using the priority provider's rate regardless of its
>    individual plausibility.

**What is unchanged**: when only one provider is configured or healthy for
a pair, B.3's priority-order-first-healthy-wins design is unmodified and
sufficient — condition 9 has no second source to compare against and does
not fire. B.7 adds two new checks to an already fail-closed system; it
narrows nothing B.6 already permits and loosens nothing.

**`sportsbook`-review confirmation (Stage 4H-B0-R5, Wave 3), no gap
found.** Checked specifically for whether B.7 needs to handle "a
sportsbook settlement priced in one asset but paid in another." It does
not, by construction, not by omission: ADR 0038 §7 and
`docs/architecture/09-sportsbook-architecture.md` §11 both confirm every
sportsbook posting (stake, payout, void, partial settlement, cashout)
resolves its asset from the wallet's own `asset_code` and "a sportsbook
bet is never silently settled in a different asset than it was staked
in" — there is no ledger-posting code path in ADR 0038 that invokes FX
conversion at all. Any cross-asset movement of sportsbook proceeds is a
separate, explicit, player-initiated `ConversionOperation` (ADR 0021),
which is an ordinary conversion B.6/B.7 already govern identically to
every other conversion on the platform — it needs no sportsbook-specific
handling because sportsbook contributes nothing special to it (no
sportsbook-specific rate, pair, or timing requirement). No change
required to B.7.

**Status, restated**: B.7, like the rest of this ADR, is **NOT
IMPLEMENTED** — no deviation-bound value, no spread-tolerance value, no
alerting mechanism, and no provider capability declaration for "issues
quote references" exists in code. This section fixes the architecture-
level contract (what must be checked, against what, and what happens on
failure); the specific numeric bounds and the alerting channel are
implementation-stage decisions, the same way B.6 item 2 leaves `max_age`'s
value to implementation. No real FX vendor is named or implied anywhere in
this section.

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
authorization design. **Correction (Stage 4H-B0-R4 Wave-2 review,
`ledger-finance`)**: the precedent this reuses from ADR 0031 §8 is only
the *negative* half — tenant-scoped roles structurally cannot reach
platform-wide data, because every non-`platform_admin` `StaffRole` is
always tenant-scoped and RLS/role-scoping enforces this mechanically.
ADR 0031 §8 itself documents, in its own words, that the *positive*
half — a working platform-admin write path for genuinely platform-wide
rules — remains an **open, unbuilt gap** there ("a genuinely
non-negotiable, tenant-proof ceiling requires a future platform-scoped
write path, not built this stage"). This ADR does not inherit that
unbuilt half: Part C requires the platform-admin write path for layers
1–3 to be built as part of implementing this authorization boundary, not
deferred the way ADR 0031 §8 deferred its own. The same shape applies
here, with the same rationale, at a larger blast radius (an asset row is
referenced by every tenant's ledger, not just one tenant's risk rules):

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

**Fail-closed default for absent configuration (Stage 4H-B0-R4 Wave-2
review correction, `security`).** Where no row exists at any layer —
no `tenant_jurisdiction_configs` row for a `(tenant_id, jurisdiction_id)`
pair (layers 4/6), or no platform-wide `AssetOperationEligibility` row
for an asset/operation (layer 7's default) — the absence **must** be
read as ineligible/deny, never as "no restriction configured, therefore
permitted." This is the identical fail-safe posture B.6 item 2 already
states for FX rate freshness ("absence of a policy is never read as 'no
limit'"), applied here to authorization rather than staleness.

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

**Non-nil error is always ineligible, no exception (Stage 4H-B0-R4
Wave-2 review correction, `security`).** A non-nil `err` from
`CheckEligibility` is treated as `eligible = false` at every call site,
with no fallback to a previously-known-good answer and no exception —
the identical convention ADR 0031 §1/§25 already states for
`risk.Evaluate` ("a non-nil error... is a DENY at every call site...
there is no sportsbook-specific softening"). This is stated explicitly
so a future caller does not treat an error path as a special case to be
handled more leniently than an ordinary denial.

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

### C.5 Administrative API surface — resolving the Stage 4H-B0-R4 deferred item (Stage 4H-B0-R5, `architect`)

Doc 27 §26.8's P1 is closed above (C.1-C.4) at the *model* level: who may
mutate which layer, and that every mutation is audited. What was still
missing, per this stage's own disclosure, is the concrete **write-side
operation catalogue** — not handler code, but which distinct
administrative operations exist, at what layer, callable by whom, and
under what control. This section defines that catalogue. No API,
handler, or migration is built here.

**C.5.1 — Canonical administrative operations.**

| # | Operation | Layer(s) touched | Caller (per C.1's two-tier split) | Audit (C.3, inherited unchanged) | Four-eyes / dual control |
|---|---|---|---|---|---|
| 1 | **Create asset** | 1 (existence) | Platform-admin only | Yes — before = NULL, after = full new row | **Required** (C.5.3) |
| 2 | **Update metadata** (mutable fields only — C.5.4) | 1 | Platform-admin only | Yes | Not required |
| 3 | **Activate** (`active` → true) | 2 | Platform-admin only | Yes | **Required** |
| 4 | **Suspend/deactivate** (`active` → false) | 2 | Platform-admin only | Yes | Not required — deliberately asymmetric, see C.5.3 |
| 5 | **Configure platform authorization** (`platform_authorized` flip — naming note below) | 3 | Platform-admin only | Yes | **Required** when granting (→ true); not required when revoking (→ false) |
| 6 | **Authorize tenant** | 4 | Tenant-scoped (tenant-configuration permission) | Yes | Not required |
| 7 | **Authorize brand** | 5 | Tenant-scoped | Yes | Not required |
| 8 | **Authorize jurisdiction** | 6 | Tenant-scoped | Yes | Not required |
| 9 | **Configure operation eligibility** | 7 | **Split**: platform-wide default row (`tenant_id IS NULL`, A.6) is platform-admin only; tenant-scoped override row is tenant-scoped | Yes | Platform-wide-default row, granting: **required**. Tenant-scoped override, and any revoking direction: not required |

**Naming note, stated because this is exactly the kind of collapse this
ADR exists to prevent**: "configure capabilities" in this task's operation
list refers to layer 3's `platform_authorized` toggle (row 5) — the
platform-wide "cleared to be tenant-facing at all" gate (A.4) — and
**must not** be confused with `ProviderCapability` (ADR 0022), a different
concept answering a different question (a *payment provider's* capability,
not the asset's own platform clearance). No operation in this table
mutates `ProviderCapability`; that table's own administrative surface is
ADR 0022's, unchanged and out of scope here.

**C.5.2 — Audit, restated once, not per-row.** Every operation above
inherits C.3's mandatory audit record (actor, tenant — NULL for a
platform-scoped mutation — entity, before/after state, IP, reason code)
unmodified. This is not a new audit design; the table's "Audit" column
exists only to confirm no operation is exempt, per C.3's own "no
exception" wording.

**C.5.3 — Four-eyes / dual control: an explicit decision, not a silent
default either way.**

CLAUDE.md requires four-eyes approval for manual balance adjustments above
a configurable threshold. Asset registry mutations are not balance
adjustments, but C.1's own reasoning already establishes that layers 1-3
carry comparable blast radius: an asset row (and its `active`/
`platform_authorized` status) is referenced by *every* tenant's ledger,
with no tenant-level checkpoint downstream capable of catching a
platform-level mistake before it takes effect everywhere at once. This ADR
decides, explicitly:

- **Dual control is required** for any operation that either (a) brings a
  **new, irreversible identity fact** into existence (create asset), or
  (b) flips a **platform-wide gate from off/absent to on** (activate;
  grant platform authorization; grant a platform-wide-default
  operation-eligibility row). Each of these is the *only* checkpoint
  standing between "not yet live anywhere" and "live for every tenant,
  immediately," per A.3's evaluation chain — there is no narrower,
  tenant-scoped gate downstream that could still catch a mistake, because
  layers 4-7 can only ever narrow what 1-3 already opened (A.5/A.6). A
  single compromised or mistaken platform-admin credential must not be
  sufficient, by itself, to make a wrong asset (wrong exponent, wrong
  type) or a not-yet-ready asset live platform-wide.
- **Dual control is deliberately NOT required for the reverse direction**
  — suspend/deactivate, revoke platform authorization, revoke a
  platform-wide eligibility row — at any layer. Turning something **off**
  is the fail-closed direction, and CLAUDE.md's own fail-closed posture
  treats "fail closed fast" as the safe default to protect, not slow
  down. Requiring a second approver on an emergency kill-switch (e.g.
  deactivating an asset mid-custody-incident, C.1's own example) would
  work directly against the incident-response need that switch exists
  for. This asymmetry — dual control going live, single-actor going dark
  — is a deliberate design choice, not an oversight.
- **Dual control is NOT required for layers 4-7's tenant-scoped
  mutations** (authorize tenant/brand/jurisdiction, tenant-scoped
  eligibility overrides), including their granting direction, because the
  narrow-only-never-widen rule (A.5/A.6) structurally bounds their blast
  radius to at most one tenant, and to at most what platform layers 1-3
  have already, separately, dual-control-approved. A compromised
  tenant-scoped actor can turn on for their tenant only what the platform
  already turned on for everyone; it cannot expose an asset platform-wide.
- This is flagged, per governance, for `security`'s explicit sign-off in
  the next review wave (per C.1's own attribution of the authorization
  boundary to `security`, and CLAUDE.md's "security-sensitive
  functionality requires explicit review by the `security` specialist"
  rule) — it is recorded here as a considered decision with stated
  reasoning, not left silently either way, but it is not self-certified.

**C.5.4 — Immutable vs. mutable fields: a hard rule, not a convention.**

**Immutable once the row is created — no operation in C.5.1's catalogue
may ever change these, at any layer, for any reason**:
- `code` (the asset's identity/primary key — every ledger and wallet
  reference is keyed on this)
- `decimal_exponent` (changing this after any ledger entry references the
  asset would silently reinterpret every existing balance's minor-unit
  meaning — CLAUDE.md's ledger-integrity rules make this unrecoverable,
  not merely undesirable)
- `asset_type` (fiat / crypto / internal-custom classification — changing
  it could silently re-route eligibility/authorization logic keyed on
  type without any new authorization decision having actually been made)
- `network` (for a network-specific crypto variant — changing it after any
  deposit-address or custody binding exists would misattribute funds to
  the wrong chain)

**Mutable, via the operations above**:
- `display_name`, `aliases`/`symbols` (op 2)
- `active` (ops 3/4)
- `platform_authorized` (op 5)
- Layer 4-6 tenant/brand/jurisdiction authorization rows (ops 6-8)
- Layer 7 `AssetOperationEligibility` rows, both scopes (op 9)

This is a **hard rule enforced by the administrative API's own operation
catalogue**, not a documentation convention an implementer could route
around: there is, by design, **no "update asset identity" operation**
anywhere in C.5.1 — `code`, `decimal_exponent`, `asset_type`, and `network`
are set exactly once, at creation (op 1), and never again by any
subsequent operation this ADR defines. An implementing stage that adds a
way to change any of these four fields after creation is not implementing
this ADR; it is contradicting it.

**C.5.5 — Minimum field set for asset creation, and what creation must
never imply.**

`Create asset` (op 1) requires, at minimum:
- `code` (identity, immutable)
- `asset_type` (immutable)
- `decimal_exponent` (immutable, 0-18 per the existing `CHECK` constraint,
  A.4)
- `network` (immutable; required when `asset_type` is a network-specific
  crypto variant, null/not-applicable otherwise)
- `display_name` (mutable display metadata)
- `aliases`/`symbols` (optional, mutable)

And **forces**, regardless of any value the caller supplies or omits:
- `active = false`
- `platform_authorized = false`
- **no** `AssetOperationEligibility` row is created for any operation, at
  any scope — the asset starts with **zero** eligibility rows, which
  C.1's fail-closed-default-for-absent-configuration rule already reads
  as "ineligible for everything," correctly, with no additional code
  needed to enforce it

**Stated explicitly and emphatically, because it is the entire point of
this section**: **asset creation must never automatically authorize any
financial operation.** Creating the layer-1 row must never imply
activation (layer 2), platform authorization (layer 3), tenant/brand/
jurisdiction authorization (layers 4-6), or any operation eligibility
(layer 7). Each of those is a **separate, deliberate administrative act**,
from C.5.1's catalogue, with its own audit record and — where C.5.3
requires it — its own dual-control approval. This is A.1's "presence in
the registry never implies depositable/withdrawable/etc." principle
(read-side, Part A), restated here on the **write** side: an API that let
`Create asset` accept an `active: true` or `platform_authorized: true`
parameter, or that auto-provisioned any eligibility row as a creation side
effect, would silently reopen exactly the gap this whole ADR exists to
close. No operation in C.5.1 accepts such a parameter; each downstream
layer's grant is reachable only through its own named operation (3, 5,
6-9), never as a flag on operation 1.

### C.6 Implementation record — Stage 4H-B0-R6, Workstream A (`architect`)

**Status change, scoped precisely.** Parts A (layers 1-7) and C are now
**IMPLEMENTED** in code and migrations. Part B (FX/Conversion) and layer 8
(market-rate availability) remain **NOT IMPLEMENTED** — Stage 4H-B0-R6 did
not authorize any FX provider work, and §S-1/§S-2 (the FX control plane's
own RBAC/dual-control/audit tier, and the circular single-provider
plausibility baseline) are untouched by this workstream and remain open
`architect`/`ledger-finance` items.

Artifacts: migrations `0044_asset_registry_failclosed_and_dual_control`
and `0045_asset_authorization_layers_4_to_7`; `internal/assetregistry`;
`internal/db.Pool.WithPlatformAdmin`; `internal/auth`'s
`PermAssetRegistryManage` / `PermAssetAuthorizationWrite`;
`internal/httpserver/asset_registry_{routes,handlers}.go`.

**C.6.1 — Four decisions that AMEND this ADR's own earlier text.** Each is
recorded here rather than edited in place above, so the original reasoning
and the correction are both visible.

1. **Layers 4 and 6 are now SEPARATE facts, superseding §A.5's "layers 4
   and 6 are the same underlying mechanism".** §A.5's reuse of
   `tenant_jurisdiction_configs.allowed_currencies` for both layers is
   incompatible with §C.2's own promise of per-layer distinguishable
   `ReasonCode`s — one row cannot produce two independent answers, which
   is exactly what `qa` found in Stage 4H-B0-R5. Layers 4, 5 and 6 are now
   three independently present-or-absent rows in a new
   `asset_authorizations` table, discriminated by `scope_kind`. The
   options considered (extending `tenant_jurisdiction_configs`; one table
   per layer; the chosen two-table split) and the tradeoffs of the chosen
   shape are documented in migration `0045`'s header comment, per this
   stage's requirement that the schema choice be recorded at the schema.
2. **`tenant_jurisdiction_configs.allowed_currencies` is no longer
   consulted for authorization**, and `CheckEligibility` deliberately does
   not read it — two mechanisms answering one question is the drift risk
   §C.2 exists to prevent. The column is NOT dropped or migrated by this
   workstream: it has zero readers in Go (verified across `internal/` and
   `cmd/`), and rewriting a human-approved Stage-1 configuration column is
   outside this stage's scope. **Follow-up required**: decide whether to
   drop it, or repurpose it as non-authorizing display configuration. This
   also resolves open question 1 (JSONB array vs. child table) by
   sidestepping it — the authoritative facts now live in a proper table
   with an FK to `assets(code)`.
3. **The operation dimension is `(product, operation)`, closing open
   question 7.** `sportsbook`'s finding is accepted, not deferred: a
   product/vertical axis is required, because real licensing regimes
   condition products separately (doc 15's `licences.permitted_products`).
   Every layer 4-7 row carries a nullable `product`; NULL means "every
   product" and a product-specific row always wins (most-specific-match),
   so a product axis can narrow but never widen. `product` is a FK to a
   new `platform_products` registry table — data, not a CHECK-constrained
   enum — so adding a product is a row, consistent with §A.1's "must be
   data, looked up, never a compiled-in switch". `operation` stays a
   six-value CHECK, because this ADR fixes that list and widening it
   should require an ADR amendment plus a migration.
   `CheckEligibility` keeps the canonical six-parameter shape §C.2 fixed;
   its `operation` parameter is now an `OperationScope{Product, Operation}`
   rather than a bare operation. **A product is REQUIRED on every check**:
   a caller that cannot name its product cannot be authorized.
4. **The seeded assets are grandfathered at layer 2 only.** Migration
   `0003`'s seven rows (EUR/USD/GBP/BRL/MXN/BTC/USDT) are set explicitly
   to `active = true` (they are already referenced by live wallet/ledger
   schema; deactivating them is a functional change this stage did not
   authorize) and explicitly to `platform_authorized = false` (nothing
   reads layer 3 yet, so fail-closed costs nothing, and §C.5.5's rule is
   really about this layer). Consequence, stated plainly: until an explicit
   dual-controlled platform-authorize act is performed per asset,
   `CheckEligibility` denies every one of the seven with
   `asset_not_platform_authorized`. That is the intended behaviour of a
   fail-closed registry on the day it is switched on, not a regression —
   and because no existing domain calls `CheckEligibility` yet, it changes
   no current behaviour.

**C.6.2 — How each Stage 4H-B0-R5 security finding was closed.**

- **S-4 (fail-open default)**: `assets.active` now defaults `false`;
  `platform_authorized` added `NOT NULL DEFAULT false`. Asserted directly
  against `information_schema` by test, so a future migration that flips a
  default back fails a test rather than silently reopening the finding.
- **S-3 (no database backstop for layers 1-3)**: closed with row-level
  security **on `assets` itself** (`ENABLE` + `FORCE`), with write policies
  requiring a platform-admin session GUC AND `app.tenant_id`/
  `app.player_account_id` to be unset — so a tenant-scoped transaction
  cannot satisfy them even if it also sets the platform GUC (tested).
  Reads stay open: the registry is reference data every money path reads
  for `decimal_exponent` with no tenant context, and PostgreSQL bypasses
  RLS for FK checks anyway. A second, independent mechanism backs it up:
  no asset can be created, activated or platform-authorized without an
  approved change request whose requester **and** approver both resolve to
  platform-scoped (`tenant_id IS NULL`) `staff_users` rows.
  **Honest residual**: the dedicated-Postgres-role option was evaluated
  and is not implementable as the platform stands — migrations run as the
  application role, which also OWNS every table (an owner can re-grant
  itself anything it revoked) and holds no `CREATEROLE`. A real
  role-separated backstop needs a second database role plus a second
  connection pool with separate credentials: **recorded here as a
  follow-up**, and the GUC-based guard is precedent-consistent with the
  platform's entire isolation model (`app.tenant_id` binds a code path the
  same way).
- **S-5a (four-eyes asserted, not enforced)**: `asset_change_requests` +
  `asset_change_approvals`, mirroring migration `0026`/`0029`'s withdrawal
  precedent — `UNIQUE (request_id, approver_principal_id)`, approval
  immutability via the same `ledger_deny_mutation()` trigger, a
  self-approval guard that blocks both the same principal and the same
  person reached through `staff_users.person_id`, and request-payload
  immutability (so an approved request cannot be rewritten before being
  applied). The consuming trigger marks the request `applied` inside the
  same statement as the mutation, so one approval authorizes one mutation,
  once. The create/self-authorize/activate/use chain is refused at every
  link (tested end to end, including over HTTP).
  **CORRECTED BY §C.7.1 — this bullet was wrong when written.** The
  same-person half of that self-approval guard could never fire, because
  nothing in this platform can set `person_id` on a `platform_admin`
  account, so the control was in fact "two distinct staff UUIDs" and
  `security` reproduced the full unilateral chain live. It also mirrored
  migration `0029`, which migration `0034` had already superseded for
  precisely this reason. Read §C.7.1 before relying on anything in this
  bullet. Left in place, uncorrected in substance, as the record of what
  round 1 claimed.
- **S-6a (zero jurisdiction silently skipped)**: a zero-value tenant or
  jurisdiction is an immediate deny with its own `ReasonCode` and a
  non-nil error. A zero brand remains permitted per §C.2 (layer 5 only
  narrows layer 4, so skipping it cannot widen anything).

**C.6.3 — Additional controls implemented beyond the findings.**

- §C.5.4's immutable identity fields (`code`, `decimal_exponent`,
  `asset_type`, `network`, plus `created_at`) are enforced by a database
  trigger, not only by the absence of an API operation. `assets` rows are
  also non-deletable (ledger history references them by code); suspension
  is the mechanism.
- Narrow-only-never-widen (§A.5/§A.6) is enforced **three** times: RLS
  (a tenant writes only its own rows), a write-time trigger (a widening
  row cannot be STORED — `security`'s explicit requirement), and
  `CheckEligibility`'s AND-chain at resolution time (so a row that was
  legal when written cannot take effect after the layer above it is
  revoked).
  **QUALIFIED BY §C.7.3**: all three mechanisms only ever see a row being
  written or read. None of them sees a row being **removed**, and
  migration 0045's `FOR ALL` policy permitted DELETE despite its own
  comment claiming otherwise — so deleting a brand-level denial widened
  eligibility past all three. Closed in migration 0047.
- Every mutating operation writes an `internal/audit` record in the same
  transaction, with actor, tenant (NULL for platform-scoped), entity,
  before/after state and a **mandatory** `reason_code` (a missing reason
  code is a 400, not an empty audit field).
- RBAC: `asset_registry:manage` (platform-only, `RolePlatformAdmin`) for
  layers 1-3 and platform-wide layer-7 defaults; `asset_authorization:write`
  (tenant-scoped, `RoleTenantAdmin`) for layers 4-7. Holding the
  tenant-scoped permission grants nothing at the platform tier (tested
  against every layer-1-3 endpoint). This resolves open question 2's
  "exact RBAC permission name(s)".
- **Player-scoped transactions can READ layers 4-7, never write them.**
  A player-initiated financial operation runs under
  `db.Pool.WithPlayerScope`, and CLAUDE.md requires the authoritative read
  to happen in the same transaction as the write it authorizes. Without a
  read policy for that scope, `CheckEligibility` would see zero rows and
  deny everything on the player path — fail-closed, but a *false* denial,
  and one that would push a future implementer toward resolving
  eligibility in a separate transaction (the stale-read pattern the
  same-transaction rule exists to prevent). Migration 0045 therefore
  carries a `player_read` SELECT policy on both tables; every write policy
  still requires `app.player_account_id` to be unset, so no player-facing
  path can alter configuration (tested both directions).
- No HTTP endpoint EVALUATES eligibility. `CheckEligibility` is an
  in-process service only, because an endpoint would have to accept a
  jurisdiction identifier from a caller and no per-player jurisdiction
  resolver exists yet (Stage 4G-FINAL Part C).

**C.6.4 — What is NOT done, explicitly.**

- **Layer 8 is NOT IMPLEMENTED and is likely not a stored fact at all.**
  §A.7 already says the Registry only exposes whether a rate-source
  binding is *configured*; whether a rate is currently valid is evaluated
  live. On implementing layers 1-7 it is now clear that layer 8 is
  substantially a **runtime FX-provider-health/freshness check** (Part B's
  §B.6 conditions 1/2/6 plus §B.7), not a row the Registry can hold. No
  `asset_rate_source_bindings` table was created; creating one before any
  `FXRateProvider` interface exists would be speculative schema. This is a
  clarification of §A.2's layer-8 row, not a contradiction of it.
- **No FX provider, Conversion Service, rate-source binding, deviation
  bound or spread bound exists.** Part B remains architecture only.
- **`aliases`/`symbols`** (§A.4's optional metadata) were not added — no
  consumer needs them, and adding unused columns is the scope expansion
  CLAUDE.md warns against. `display_name` is the one mutable metadata
  field.
- **No existing domain calls `CheckEligibility` yet.** Wiring wallet,
  payments, casino, withdrawal and (future) sportsbook/FX call sites to
  the canonical service is deliberately a separate, per-domain change:
  each call site needs a server-resolved jurisdiction, which does not
  exist yet. Until that wiring happens, the authorization boundary is
  available and enforced *where called*, and §C.2's "every downstream
  domain calls this one function" is an obligation on future work, not a
  claim about today's code.
- **This section is not self-certified.** Per §C.5.3's own flag and
  CLAUDE.md's rule, `security` reviews this implementation independently;
  this record states what was built and what remains open, it does not
  grant sign-off.

### C.7 Review-fix record — Stage 4H-B0-R6, Workstream A round 2 (`architect`)

`security` and `code-reviewer` reviewed §C.6's implementation
independently, as §C.6.4 said they would, and both found real defects.
This section records what they found and what migration 0047 and the
accompanying Go changes do about it. Nothing here is new scope: each fix
makes a control this ADR already *specified* actually true.

**C.7.1 — The four-eyes person-identity check was inert (P1,
launch-blocking, reproduced live).** §C.5.3 requires that two staff
accounts held by one human count as one human. Migration 0044 implemented
that as a comparison guarded by `person_id IS NOT NULL` on both sides —
mirroring migration 0029. `security` traced every path that can create a
`platform_admin` account and established that **no code path in this
platform can set `person_id` on one**: `cmd/seed-admin` passes `nil`,
`admin_routes.go`'s staff-creation role allowlist excludes
`platform_admin` entirely, and the person-link remediation route is
tenant-scoped, so `staff_users`' own dual-scope RLS hides every
`tenant_id IS NULL` row from it. The person comparison therefore could
never fire, and §C.5.3's control degraded to "two distinct staff UUIDs" —
which one operator defeats by running `seed-admin` twice. The full bypass
(file → approve through the second account → create → activate) was
reproduced end to end.

Compounding this: migration 0044 mirrored migration **0029**, but the
platform's own current precedent is migration **0034**, which explicitly
REFUSES a withdrawal decision when the approver's `person_id IS NULL` or
the approver's account is not `active`. 0034 superseded 0029 for exactly
this reason. Migration 0044 mirrored a withdrawn pattern.

Migration 0047 brings both 0044 trigger functions in line with 0034: on
the requesting side and the approving side alike, the principal must
resolve to a staff row, be platform-scoped, carry a confirmed Person
linkage, and be `active`. The person comparison is now unconditional,
because neither side may be NULL. There is deliberately **no**
service-identity carve-out of the kind 0034 has for automated withdrawal
approvals: §C.1 is explicit that layers 1-3 are reachable only by a
platform-scoped human principal, so "cannot be resolved to a staff row"
is a refusal.

> **DEPLOYMENT ORDERING DEPENDENCY — NOT OPTIONAL.** Applied alone, with
> no way to person-link a platform-scoped staff account, this fix makes
> the Asset Registry's entire administrative surface **permanently
> unusable**: no `platform_admin` could file or approve anything, so no
> asset could ever again be created, activated, platform-authorized, or
> granted a platform-wide layer-7 default. That is fail-closed, which is
> the correct direction, but it is a total outage rather than a graceful
> degradation. Migration 0047 must land **together with, or after**, a
> path that can person-link platform-scoped staff. That path is owned by
> `identity-compliance` and is NOT part of this workstream (different
> package, different owner); the candidates are a `personID` argument on
> `cmd/seed-admin` and a platform-scoped person-link route running under
> a transaction that can see `tenant_id IS NULL` staff rows. Pre-deploy
> gate: `SELECT id, email FROM staff_users WHERE tenant_id IS NULL AND
> status = 'active' AND person_id IS NULL` must return zero rows.
> `architect` has not verified that path exists; this ADR records the
> dependency rather than assuming it is satisfied.

**C.7.2 — The layer-7 platform-wide grant had no four-eyes
representation (P2, found independently by both reviewers).** §C.5.1
operation 9 and §C.5.3 both require dual control for granting a
platform-wide-default operation-eligibility row, and migration 0045's own
comment asserted that it was "dual-controlled at the API level". It was
not: migration 0044's `asset_change_requests.operation` CHECK allowed
only `create`/`activate`/`platform_authorize`, so the request type did not
exist and one compromised platform-admin credential could flip an
eligibility gate for **every tenant on the platform** in a single call.

Migration 0047 adds a fourth operation, `platform_operation_eligibility`,
and enforces it with a **payload-matched** consume: the approved request
names the exact `(asset, eligibility_operation, eligibility_product)`
being granted, so an approval for casino wagering cannot be spent on
sportsbook wagering, on casino withdrawal, or on a broader every-product
grant. Every-product is recorded as the explicit `'*'` sentinel (matching
the `COALESCE(product, '*')` convention migration 0045's own unique
indexes already use) rather than an absent field, so the approver
approves the breadth too.

Two mechanism notes worth recording, because they are not obvious and a
future change could easily get them wrong:

- The consume runs in an **AFTER INSERT OR UPDATE** trigger, not the
  existing BEFORE narrowing trigger. Layer-7 rows are written with
  `INSERT ... ON CONFLICT DO UPDATE`; PostgreSQL fires BEFORE INSERT
  first, *then* detects the conflict and fires BEFORE UPDATE, so one
  upsert fires the BEFORE trigger twice and the first firing's side
  effects are not undone. Consuming there would demand two approvals for
  one logical grant. An AFTER row trigger fires exactly once, for the row
  that survives, with the true `OLD`. A `RAISE` there still aborts the
  statement, so the control is no weaker.
- Scope of the requirement: every INSERT of a platform-wide eligible row,
  and any UPDATE that turns one on, re-points it at a different
  asset/operation/product, or promotes a tenant row to a platform row. An
  idempotent re-write of an already-eligible row does not need a new
  approval (mirroring `assets_enforce_dual_control`'s off→on rule), and a
  **revocation is never dual-controlled** — §C.5.3's asymmetry, for the
  same reason suspend/revoke are single-actor at layers 2-3.

**C.7.3 — `asset_authorizations` RLS permitted DELETE (P2, reproduced
live).** Migration 0045's comment said "No DELETE policy, deliberately"
and then created `tenant_isolation` as `FOR ALL`, which includes DELETE.
`security` proved the consequence: deleting a brand-level
`eligible = false` row silently promotes that brand from denied to
allowed, because §A.5's nullable-narrowing pattern makes an **absent**
layer-5 row mean "inherit the tenant answer". The narrowing trigger cannot
see it (it is BEFORE INSERT OR UPDATE) and nothing is written to the audit
trail. Migration 0047 splits the policy per command — SELECT / INSERT /
UPDATE, none for DELETE — matching the shape
`asset_operation_eligibility` already had.

Two deliberate boundaries on this fix:

- **No BEFORE DELETE deny-trigger.** `asset_authorizations.tenant_id`
  declares `ON DELETE CASCADE`, and PostgreSQL runs referential-integrity
  actions with RLS bypassed, so deleting a tenant still removes its
  authorization rows. A deny-trigger would contradict the table's own
  declared cascade. (`assets` declares no cascade, which is why it *does*
  carry such a trigger.) Regression-tested both ways.
- **TRUNCATE is closed too**, with a statement-level trigger on both
  layer-4-7 tables. RLS does not apply to TRUNCATE at all — it is an
  owner-level operation and the application role owns these tables — so
  the policy split cannot cover it, and one statement would otherwise
  erase every authorization and eligibility row on the platform, turning
  every absent-row denial into an inherit. Migration 0044 already carries
  this exact guard for `assets`, `asset_change_requests` and
  `asset_change_approvals`; migration 0045's two tables were left without
  one.

**C.7.4 — Two low-severity correctness fixes.** `CheckEligibility`'s doc
comment claimed "a non-nil error ALWAYS accompanies `eligible == false`",
which is backwards: every ordinary layer denial returns
`(false, reason, nil)`. The invariant is one-directional — `eligible` is
never true when `err != nil`, and a denial may carry a nil error. A caller
who believed the old wording would have been entitled to treat a
fail-closed denial as a non-answer and retry past it. And
`DecideChangeRequest` classified any `23505` as self-approval, when the
only reachable unique constraint is
`UNIQUE (request_id, approver_principal_id)` — i.e. a duplicate
submission. Duplicate decision now has its own sentinel, so a retrying
operator is not accused of self-dealing; genuine self-approval still comes
from the trigger as a distinct condition.

**C.7.5 — Migrations 0044 and 0045 are NOT edited in place.** They are
applied migrations; rewriting them would make the live schema a different
thing from what the migration chain says it is. Migration 0047's down
migration restores 0044/0045's original definitions, with one stated
limitation: the restored narrow `operation` CHECK is added `NOT VALID`,
because APPLIED `platform_operation_eligibility` request rows created
while 0047 was in force cannot be validated without either destroying
audit history (`asset_change_requests` carries a deny-delete trigger for
exactly that reason) or rewriting an immutable column to something untrue.
The constraint is fully enforced for every new row; only pre-existing
history is left unvalidated. Migration 0044's own down file already states
this class of limitation for the seven seeded asset rows.

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

- Status: **superseded by §C.6 for Parts A and C.** As originally
  recorded, no migration, admin API, RBAC permission, audit-log wiring,
  `AssetAuthorization` service, FX Rate Provider adapter, or Conversion
  Service existed. Stage 4H-B0-R6 (Workstream A) built the migrations,
  admin API, RBAC permissions, audit wiring and `AssetAuthorization`
  service for layers 1-7 (§C.6). **No FX Rate Provider adapter and no
  Conversion Service exists** — Part B is still architecture only.
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

**Status after Stage 4H-B0-R6, Workstream A (see §C.6):** questions 1, 2,
4 and 7 are **RESOLVED** — 1 by moving the authoritative facts out of
`allowed_currencies` into a proper table with an FK to `assets(code)`
(§C.6.1 item 2), 2 by naming `asset_registry:manage` /
`asset_authorization:write` (§C.6.3), 4 by the `asset_authorizations`
`scope_kind = 'brand'` row shape plus a write-time narrowing trigger, and
7 by adding the `(product, operation)` dimension (§C.6.1 item 3).
Questions 3, 5 and 6 remain **OPEN**: 3 and 5 are Part B (no FX work was
authorized), and 6 is `security`'s independent sign-off on the four-eyes
gating, which §C.6 implements but does not self-certify.

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
5. **Magnitude/spread bound values** (B.7.2/B.7.3, added Stage 4H-B0-R5)
   — this ADR fixes that a deviation bound and a cross-provider spread
   bound must exist and must fail closed when unconfigured; the actual
   numeric defaults and per-pair overrides are `ledger-finance`/`risk`
   implementation-stage work, mirroring how B.6 item 8's financial-
   constraint hook is likewise left unvalued here.
6. **Four-eyes gating decision** (C.5.3, added Stage 4H-B0-R5) — this ADR
   decides which administrative operations require dual control and
   states its reasoning; it is explicitly flagged for `security`'s
   independent sign-off, not self-certified.
7. **No product/vertical dimension in `CheckEligibility`'s `operation`
   value or in the reused `tenant_jurisdiction_configs.allowed_currencies`
   (layer 6)** (`sportsbook`-review finding, Stage 4H-B0-R5, Wave 3).
   `AssetAuthorization.CheckEligibility`'s `operation` enum
   (`deposit | withdrawal | wagering | settlement | conversion |
   reporting`, C.2) and layer 6's reused `allowed_currencies` array (A.5)
   are both scoped to `(tenant, brand, jurisdiction, asset)` with no
   product/vertical axis — a single `wagering` eligibility answer applies
   identically to a casino bet and a sportsbook bet placed by the same
   tenant, in the same jurisdiction, in the same asset. This is in tension
   with a fact already modeled elsewhere on this platform:
   `docs/architecture/15-jurisdiction-and-licensing-model.md`'s `Licence`
   row carries `permitted_products` (jsonb: casino/sportsbook/etc.) *per
   jurisdiction*, because real licensing regimes routinely license casino
   and sports betting as distinct, separately-conditioned products (a
   jurisdiction's sports-betting licence terms can restrict a settlement
   currency/asset differently than its casino licence, independent of
   which asset the platform has otherwise made wagering-eligible). As
   specified, `CheckEligibility` cannot express "BTC is wagering-eligible
   for casino but not for sportsbook in jurisdiction X" — there is no
   input parameter for it to key on. This may be a deliberately deferred
   scope decision (no product currently needs the distinction) rather than
   an oversight, but it is not stated as such anywhere in this ADR, and
   `sportsbook` is not positioned to decide whether it is acceptable to
   defer — flagged for `architect` (does layer 6/7 need a product/vertical
   parameter, added the same narrow-only-never-widen way brand narrows
   tenant) and for confirmation against `docs/decisions/0006-hybrid-
   licensing-and-jurisdiction-model.md`'s and doc 15's product-licensing
   model before this ADR's eligibility surface is treated as complete for
   a multi-product tenant.

These are handed to `ledger-finance` (financial correctness of Part B),
`security` (Part C's authorization/audit design), and `qa`
(extensibility — confirming no new code path anywhere hardcodes an
asset, an operation list, or a layer count) for the second-wave review
this stage's directive specifies.

## Owner

`architect` (cross-domain boundary and layering design), `ledger-finance`
(Part B's financial correctness once implementation begins), `security`
(Part C's authorization boundary).
