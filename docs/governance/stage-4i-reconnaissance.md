# Stage 4I — Platform-wide Jurisdiction Resolution Foundation — Reconnaissance Report

**Status of this document: READ-ONLY RECONNAISSANCE.** No code, migration,
schema, or other architecture document was modified to produce it. It is the
mandatory first action of the human-authorized "Stage 4I — Platform-wide
Jurisdiction Resolution Foundation" directive, mirroring the role
`docs/governance/wave-3-reconnaissance.md` played at the start of Stage 4H-B1
Wave 3: a **from-code, not from-docs** reconstruction of the jurisdiction
architecture as it actually exists today, an explicit producer/consumer
inventory, a dependency map, the contradictions between domains' current
jurisdiction assumptions, candidate Human Decision Register items (named, not
decided), and a set of open design questions for the next specialist phase.

**This document deliberately does NOT propose a resolution interface or data
model.** Per the directive, that is the next phase's job
(`identity-compliance` first, then `risk`, `security`, `backend`, `casino`,
`bonus-engine`, `payments`, `sportsbook`, `qa`, `architect` final, then
independent security/compliance final), done collaboratively. §9 below
contains one explicitly-labelled *illustrative, not decided* sketch, included
only to make the design tensions concrete — it is not a ruling and must not
be treated as one.

**Method.** Every jurisdiction-bearing file in the repository was read
directly at HEAD `cb70d7a` (branch `claude/focused-wright-jw88w9`, working
tree clean, `go build ./...` and `gofmt -l .` both clean at the start of this
reconnaissance — the repository is in the state `wave-3-report.md` describes,
not drifted). A repo-wide search for `jurisdiction` (case-insensitive) found
**861 matches across 89 `.go`/`.sql` files** and **~700 across 71 docs**;
every non-test code match and every migration match was read in context,
plus: `docs/governance/wave-3-report.md` (in full), ADR 0006, ADR 0031
§9/§10/§34, ADR 0034 §8/§14, ADR 0037 §A.3/§A.5/§C.2/§C.6.4 and its open
question 7, ADR 0039, `docs/architecture/15-jurisdiction-and-licensing-model.md`,
and the jurisdiction sections of docs 02, 09, 11, 16, 26, 37. Every claim
below marked "confirmed by code" was checked against the live repository,
never inherited from a prior report's summary.

---

## 1. From-code reconstruction — the jurisdiction architecture as it exists today

### 1.1 Schema inventory: every jurisdiction-bearing column, and whether it is actually populated

| # | Table.column | Migration | What it is | Production writer? | Production reader? |
|---|---|---|---|---|---|
| 1 | `jurisdictions (id, code, name, regulatory_body)` | 0002 | The platform-level jurisdiction registry. No `tenant_id`, no RLS — a platform fact like `assets` | **None.** No HTTP route, no Go function inserts a row (confirmed: only test fixtures do) | One query only: `internal/bonus/eligibility.go:144` (`SELECT id FROM jurisdictions WHERE code = $1`), plus FK targets |
| 2 | `licences (jurisdiction_id, licensee, permitted_products, permitted_markets, status, expires_at)` | 0002 | The licence a tenant operates under, with its permitted products and permitted markets | **None** | **None.** Zero production Go reads |
| 3 | `tenants.licence_id` (+ `expected_licensee` generated col, composite FK) | 0002, 0007, 0017 | Binds a tenant to its licence, DB-enforced against `licensing_model` | **None** | **None** |
| 4 | `tenants.licensing_model` (`under_platform_licence` \| `own_licence`) | 0001 | ADR 0006's hybrid-licensing discriminator | Tenant provisioning | **Yes** — `resolveLicensingMode` in `internal/casino` and (re-implemented) `internal/bonus/eligibility.go:160` |
| 5 | `tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc/aml/rg/reporting_ruleset_id, allowed_currencies, allowed_payment_methods, geo_block_list, effective_from/to)` | 0002, RLS added 0005 | Doc 15's central per-tenant, per-jurisdiction regulatory configuration | **None** | **None.** Zero production Go reads or writes (confirmed by repo-wide grep — only `internal/db/tenant_rls_integration_test.go` touches it, as an RLS fixture) |
| 6 | `casino_games.jurisdiction_blocklist TEXT[]` | 0035 | Platform-level per-game jurisdiction blocklist | Casino admin catalogue upsert | `internal/casino/orchestrator.go:138` — but **only when `params.JurisdictionCode != nil`**, which never happens in production (§1.3) |
| 7 | `casino_launch_sessions.jurisdiction_code` (FK → `jurisdictions.code`, write-once via immutability trigger) | 0042 | Stage 4G-FINAL Part C: the jurisdiction resolved at launch, denormalized so `postBet` can reuse it | `internal/casino/launch.go:124` — inserted as `NULLIF($11, '')`, so **always NULL in production** | `internal/casino/orchestrator.go:728` (`postBet`'s `RiskRequest`) |
| 8 | `risk_rules.jurisdiction_code` (FK → `jurisdictions.code`), `risk_rules.licensing_mode` | 0041, 0042 | Optional scope dimensions on a Risk rule; immutable after creation | `internal/risk/policy_service.go:200` (staff-authored rules via `POST /v1/admin/risk/rules`) | `internal/risk/evaluator.go:174` (`Rule.matches`), `:223` (`missingScopeContext`) |
| 9 | `asset_authorizations.jurisdiction_id` (FK → `jurisdictions.id`), `scope_kind='jurisdiction'`, + `product` | 0045 | ADR 0037 layer 6: is this asset permitted for this `(tenant, jurisdiction[, product])`? | `internal/assetregistry/authorization_admin.go:68` `AuthorizeScope` (staff, `asset_authorization:write`) | `internal/assetregistry/authorization.go:163-172` (`CheckEligibility` layer 6) |
| 10 | `open_bet_self_exclusion_policies.jurisdiction_code` (FK → `jurisdictions.code`, NOT NULL) | 0043, 0049 | ADR 0034 §14: jurisdiction-primary, tighten-only self-exclusion policy floor | **None** — `SetOpenBetSelfExclusionPolicy` has zero production callers and no HTTP route | **None** — `ResolveOpenBetSelfExclusionPolicy` likewise has zero production callers |
| 11 | `withdrawal_policies.jurisdiction_code` | 0032, constrained 0033 | A jurisdiction dimension on withdrawal approval policy | **Structurally impossible** — migration 0033 adds `CHECK (jurisdiction_code IS NULL)`, deliberately disabling it "until real jurisdiction resolution exists" | `internal/withdrawal/policy.go:99` — the query supports it; every caller passes `nil` |
| 12 | `bonus_grants.jurisdiction_code` (FK → `jurisdictions.code`, immutable) | 0057 | The jurisdiction a Grant was evaluated under | **None.** `Grant.JurisdictionCode *string` (`internal/bonus/grant.go:76`) is never assigned by any production code path — always NULL (confirmed by grep: no `Grant{...JurisdictionCode:...}` literal and no `g.JurisdictionCode =` outside tests) | Scanned back only |
| 13 | `bonus_campaign_versions.jurisdiction_restriction TEXT[]` | 0054 | Campaign-level jurisdiction restriction | Campaign version create | **Stored, never read** by any business logic |
| 14 | `bonus_offer_versions.eligibility_jurisdiction_list TEXT[]` | 0055 | Offer-level jurisdiction eligibility axis | Offer version create | **Stored, never read** by any business logic (already named in `wave-3-reconnaissance.md` §2 item 15; still true at HEAD) |
| 15 | `payment_provider_capabilities.supported_countries TEXT[]` | payments | ISO **country** codes a PSP may serve | Capability config (narrow-only vs. adapter declaration) | Declared-set narrowing check only (`internal/payments/capability.go:306`). **Not used in routing** — `RouteProvider` implements dimensions 1,3,4,5,6 and explicitly omits dimension 2 (`internal/payments/orchestrator.go:83-103`, `TODO(jurisdiction)`) |
| 16 | `kyc_documents.issuing_country` | 0040 | The country that issued a KYC document | KYC document upload | Stored/returned; **never mapped to a jurisdiction** |
| 17 | `sessions.ip_address`, `login_attempts.ip_address`, `audit_log.ip_address` (`INET`) | 0012, 0013, 0014 | Raw IP capture | Yes | Never resolved to a country or jurisdiction anywhere |

**There is no player-level jurisdiction, country, residence, or nationality
column anywhere in the schema.** Confirmed directly: `persons` (migration
0009) carries only `status` and `person_key_hash`; `player_accounts`
(migration 0010) carries no geographic field at all and its own comment
states "No name/address/document data — that belongs to the Stage 4 KYC
subsystem". `brands` (migration 0008) carries **no** jurisdiction column
either. `registration_channel` — which `docs/architecture/11-kyc-aml-rg-
architecture.md` §1 refers to as an *input* to a jurisdiction rule — **does
not exist in the codebase at all** (zero occurrences in `.go`/`.sql`).

### 1.2 Jurisdiction is currently represented three incompatible ways

1. **`jurisdictions.id` (`uuid.UUID`)** — what `assetregistry.AssetAuthorization.CheckEligibility` takes
   (`internal/assetregistry/authorization.go:69-76`). `uuid.Nil` is an
   **immediate, unconditional denial** with `ReasonJurisdictionContextMissing`,
   explicitly "never treated as 'this check is not scoped to a jurisdiction'"
   (its own doc comment, closing security finding S-6a).
2. **`jurisdictions.code` (`string`)** — what `risk.RiskRequest.JurisdictionCode`
   (`internal/risk/types.go:320`), `casino.LaunchGameParams.JurisdictionCode`
   (`*string`), `casino_launch_sessions.jurisdiction_code`,
   `rg.SetOpenBetSelfExclusionPolicyParams.JurisdictionCode`,
   `withdrawal.ResolveApprovalPolicy`'s `jurisdictionCode *string`, and every
   Bonus `JurisdictionCode string` parameter take.
3. **ISO country code (`string`)** — what `payments`' `SupportedCountries`
   (`internal/payments/types.go:188`) takes. Doc 15's own example code set is
   `'KM-ANJ'`, `'MT'`, `'CO'` — i.e. the jurisdiction code space is **not**
   the ISO-3166 country space (`KM-ANJ` is a sub-national regulator code),
   and **no mapping table between the two exists**.

Only one place in the whole repository bridges (1) and (2):
`internal/bonus/eligibility.go:139-152`'s package-private
`resolveJurisdictionID`, which does `SELECT id FROM jurisdictions WHERE code = $1`
and returns `uuid.Nil` for both an empty code and an unknown code. Casino,
Risk, RG and Withdrawal never do this lookup at all.

### 1.3 The actual end-to-end flow, per domain, as it runs today

**Casino (real-money launch → bet).** `POST /v1/casino/games/{gameID}/launch`
(`internal/httpserver/casino_handlers.go:182-184`) constructs
`casino.LaunchGameParams` from the authenticated session — and **never sets
`JurisdictionCode`**, leaving it `nil`. Consequences, all confirmed by code:

- `orchestrator.go:138`'s `casino_games.jurisdiction_blocklist` check is
  skipped entirely (`params.JurisdictionCode != nil &&`). The per-game
  jurisdiction blocklist is therefore **unreachable in production** — this
  is a **fail-open** on a legal/market-availability control, documented as
  such in `LaunchGameParams`' own doc comment ("A nil value skips the
  jurisdiction check entirely").
- `orchestrator.go:166-168` collapses `nil` to `""`, evaluates
  `risk.Evaluate` with `JurisdictionCode: ""`, and passes `""` to
  `CreateLaunchSession`, which stores `NULLIF($11,'')` → **NULL**
  (`launch.go:124`). Every production launch session carries no jurisdiction.
- `postBet` (`orchestrator.go:705-728`) reads `session.JurisdictionCode` back
  — structurally correct, permanently empty in practice.
- At Risk, an empty request-side `JurisdictionCode` is **conditionally
  fail-closed**: `risk.ErrMissingJurisdiction` fires only if at least one
  *jurisdiction-scoped rule exists for that operation*
  (`internal/risk/evaluator.go:223`). With no such rule authored, the bet
  proceeds normally. So today casino neither enforces nor blocks on
  jurisdiction — it simply has none.
- Casino does **not** call `AssetAuthorization.CheckEligibility` at all
  (confirmed: `internal/assetregistry` is imported by `internal/bonus`,
  `internal/money`, `internal/risk/denomination.go`, `internal/httpserver`
  and tests only). The unconditional fail-closed jurisdiction gate therefore
  does not bite the bet path.

**Bonus (the domain actually blocked).** `bonus.GateCheckpoint`
(`eligibility.go:79-120`) is the only production consumer of
`CheckEligibility`. It needs **both** representations at once:
`GateParams.JurisdictionID uuid.UUID` for AssetAuthorization and
`GateParams.JurisdictionCode string` for Risk (`eligibility.go:53-54`).
`IssueGrant`/`ActivateGrant`/`ConvertGrant`/`ResolveHeldDispositionAction`
each derive the former from the latter via `resolveJurisdictionID`.

Where does `JurisdictionCode` come from?
- **System-initiated sweeps**: hardcoded `""`. `deposit_sweep.go:508` and
  `cashback_scheduler.go:331` both pass `JurisdictionCode: ""`, with an
  explicit, honest "NAMED, DISCLOSED LIMITATION" comment at
  `deposit_sweep.go:435-449`. `""` → `uuid.Nil` → **unconditional
  AssetAuthorization denial** at `ReasonJurisdictionContextMissing`. This is
  precisely the blockage `wave-3-report.md` §4/§22 carries forward.
- **Staff-initiated operations**: taken from the **HTTP request body** —
  `jurisdiction_code` is a JSON field on `issueManualGrantRequest`
  (`bonus_handlers.go:399`), `resolveHeldDispositionRequest`
  (`bonus_handlers.go:633`), `issueManualGrantOpRequest`
  (`bonus_domain_ops_handlers.go:497`), the activate-manual-grant request,
  and the bulk-job-execute request (`:719`). This is a caller-supplied value
  that selects which jurisdiction-scoped Risk rule and which
  `asset_authorizations` jurisdiction row apply — see contradiction **C-4**.
- **Player-initiated coupon redemption** (`RedeemCoupon`): the handler never
  populates it, so `""` again.

**RG.** `internal/rg.EvaluateEligibility` — the platform's single "may this
player gamble right now" boundary — is entirely **jurisdiction-unaware** by
design (`internal/rg/rg.go:14` records "jurisdiction-at-player-level" as an
explicitly deferred extension point of ADR 0026). The one jurisdiction-keyed
RG artefact that exists, `open_bet_self_exclusion_policies` (ADR 0034 §14),
is a configuration primitive with **zero production callers on either side**.

**Payments / Withdrawal.** Routing dimension 2 (jurisdiction/country) is an
open `TODO(jurisdiction)` (`internal/payments/orchestrator.go:98-103`), and
`withdrawal_policies.jurisdiction_code` is hard-disabled at the database
level by `CHECK (jurisdiction_code IS NULL)` (migration 0033), with the
comment "Fail closed identically, until real jurisdiction resolution
exists." That `CHECK` is the cleanest existing precedent for how this
platform has so far handled the gap, and any resolver work must plan to lift
it deliberately.

**Sportsbook / Retail / Back Office / Partner Console / B2C frontend.** Not
implemented. `internal/agentnetwork`, `internal/segment`, `internal/crm`,
`internal/affiliate` do not exist. Doc 26 §1.4 records that a future
`hierarchy_nodes.jurisdiction_code` (a registered shop address) "would be the
first authoritative, non-guessed, non-geolocated source of jurisdiction this
platform has ever had" — a forward-looking constraint on this stage's design,
not a dependency of it.

### 1.4 Net finding, stated precisely

**Every jurisdiction *configuration* surface on this platform exists;
no jurisdiction *resolution* producer exists.** The registry
(`jurisdictions`), the licensing model (`licences`,
`tenants.licensing_model`), the per-tenant regulatory configuration
(`tenant_jurisdiction_configs`), the per-asset jurisdiction authorization
(`asset_authorizations`), the per-rule jurisdiction scope
(`risk_rules.jurisdiction_code`), the per-jurisdiction RG floor
(`open_bet_self_exclusion_policies`) and the per-operation snapshot columns
(`casino_launch_sessions.jurisdiction_code`, `bonus_grants.jurisdiction_code`)
are all built, migrated and RLS-correct. **Nothing computes which
jurisdiction applies to a given player performing a given operation**, and
therefore — with one partial exception, staff supplying it in a request body
— nothing ever populates any of them with a real value.

Secondary finding, not previously stated this precisely anywhere: **three of
these tables have never been read or written by production code at all**
(`licences`, `tenants.licence_id`, `tenant_jurisdiction_configs`). Doc 15
describes `TenantJurisdictionConfig` as the central enforcement point for
geo-gating, KYC/AML/RG ruleset resolution, payment filtering and reporting
format selection. **None of those five enforcement points is wired to it.**
Migration 0045's own header further records that
`tenant_jurisdiction_configs.allowed_currencies` is now "redundant" for the
asset-availability question, superseded by `asset_authorizations` — so part
of doc 15's stated model has already been silently partially replaced,
without doc 15 being updated to say so.

---

## 2. The directive's seven concept distinctions vs. what the code actually has

The directive requires that the following seven be distinguished and **not
conflated**. Status of each, from code:

| # | Concept | Exists in code today? | Where it is (or would be) | Conflation risk found |
|---|---|---|---|---|
| 1 | **Player location** (where the player physically is at operation time) | **NO.** No geolocation of any kind. `sessions.ip_address`/`login_attempts.ip_address` are stored but never resolved to a country | — | Nothing conflates it because nothing has it |
| 2 | **Player residence** (declared/verified home jurisdiction) | **NO.** `persons` and `player_accounts` carry no address, country or residence field; `docs/architecture/16-privacy.md` deliberately kept PII off `player_accounts` | Would most plausibly attach to `persons` (platform-scoped, cross-brand) or `player_accounts` (tenant-scoped) — **that choice is itself undecided and has RLS/privacy consequences** | — |
| 3 | **Nationality** | **NO.** The closest artefact is `kyc_documents.issuing_country` (migration 0040), which is the issuing state of a document — *not* nationality, and never read as one | — | **Latent**: `issuing_country` is the only country-shaped player fact in the system and is the obvious thing a future implementer would reach for. It is not nationality, not residence, and not location |
| 4 | **Tenant licensing jurisdiction** | **PARTIAL.** `licences.jurisdiction_id` exists but is never read; `tenants.licensing_model` (the *mode*, not the jurisdiction) is read | `licences`, `tenants.licence_id` | **Yes — C-1** |
| 5 | **Brand operating jurisdiction** | **NO.** `brands` has no jurisdiction column | `tenant_jurisdiction_configs` is keyed `(tenant_id, jurisdiction_id)` with **no brand dimension**, yet `asset_authorizations`, `risk_rules` and `open_bet_self_exclusion_policies` all carry a brand dimension alongside jurisdiction | **Yes — C-2** |
| 6 | **Transaction/operation jurisdiction** | **PARTIAL, and only as a snapshot slot.** `casino_launch_sessions.jurisdiction_code` (always NULL) and `bonus_grants.jurisdiction_code` (always NULL) are the two operation-level snapshot columns that exist | — | **Yes — C-6** (casino snapshots at *launch* and reuses for the whole round; Bonus resolves *per checkpoint*; these are different temporal semantics with no shared rule) |
| 7 | **Product jurisdiction** (a licence permits casino but not sportsbook in jurisdiction X) | **PARTIAL.** `licences.permitted_products` exists (never read). `asset_authorizations` *does* carry a `product` column with most-specific-match semantics (migration 0045:173), and `CheckEligibility` passes `scope.Product` into layer 6 | ADR 0037 open question 7 flags the absence of a product axis as unresolved and routed to `architect` — **that doc is stale relative to the code**, which grew the axis in migration 0045 | Doc/code drift, not conflation |

**Answer to the directive's framing question:** the current code does not
conflate concepts 1–3 with each other, because **none of the three exists**.
What it does do is worse in one specific way: *every* consumer takes a single
opaque `jurisdiction_code`/`jurisdiction_id` with **no recorded basis** for
what that value means. There is no field anywhere recording *why* a given
jurisdiction was chosen, so as soon as more than one source exists, the
conflation becomes invisible and unauditable. That is the single most
important structural property the next phase should decide on (see Q-1).

---

## 3. Producers — every place a jurisdiction value is set or computed today

Exhaustive. "Producer" = anything that originates a jurisdiction value, as
opposed to storing or reading one.

| P# | Producer | Kind | What it produces | Status |
|---|---|---|---|---|
| P-1 | *(none)* — player-jurisdiction resolution | — | — | **DOES NOT EXIST.** This is the gap |
| P-2 | `casino.LaunchGameParams.JurisdictionCode` supplied by its caller | Parameter | `*string` code | Contract says "MUST be resolved server-side… NEVER from client-supplied input". **The one production caller (`casino_handlers.go:182`) never sets it** → always `nil` |
| P-3 | Staff HTTP request body, Bonus surfaces | Client-supplied | `jurisdiction_code` string | **LIVE.** `bonus_handlers.go:399`/`:633`, `bonus_domain_ops_handlers.go:497`/`:546`/`:719`/`:806`. The only path by which a non-empty jurisdiction reaches a gate in production. See **C-4** |
| P-4 | Bonus sweeps/schedulers | Hardcoded | `""` | **LIVE.** `deposit_sweep.go:508`, `cashback_scheduler.go:331`. Honest, disclosed, fail-closed |
| P-5 | `bonus.resolveJurisdictionID` | Translator (not an originator) | `code → uuid` | **LIVE**, package-private to `internal/bonus`; returns `uuid.Nil` for both empty and unknown codes |
| P-6 | `resolveLicensingMode` (casino + bonus, duplicated) | Lookup | `tenants.licensing_model` | **LIVE and correct** — the only jurisdiction-adjacent value that is genuinely server-resolved on every operation today |
| P-7 | Staff authoring a `risk_rules` row | Configuration write | `jurisdiction_code` scope on a rule | **LIVE** (`risk_handlers.go:244`). Configuration, not resolution |
| P-8 | Staff authoring an `asset_authorizations` jurisdiction row | Configuration write | `jurisdiction_id` scope | **LIVE** (`authorization_admin.go:68` `AuthorizeScope`). Configuration, not resolution |
| P-9 | `rg.SetOpenBetSelfExclusionPolicy` | Configuration write | `jurisdiction_code` floor | **NO PRODUCTION CALLER, no HTTP route** |
| P-10 | Casino catalogue upsert | Configuration write | `casino_games.jurisdiction_blocklist` | **LIVE**; the consuming check is unreachable (§1.3) |
| P-11 | Admin surface for `jurisdictions` / `licences` / `tenant_jurisdiction_configs` | Configuration write | The registry itself | **DOES NOT EXIST.** No HTTP route, no Go writer. Rows can only be created by direct database access |

**Summary: there is exactly one live producer of a non-empty operational
jurisdiction value on this platform (P-3), and it is a staff-supplied request
body field.** Everything else is either configuration-side, hardcoded empty,
or non-existent.

---

## 4. Consumers — every place a jurisdiction value is read or relied upon

| C# | Consumer | What it does with it | Behaviour when absent | Live today? |
|---|---|---|---|---|
| K-1 | `assetregistry.CheckEligibility` layer 6 | Looks up `asset_authorizations` for `(tenant, jurisdiction[, product])`; absence of a row = not authorized | **`uuid.Nil` ⇒ unconditional denial**, `ReasonJurisdictionContextMissing` | **Yes** — via Bonus only |
| K-2 | `risk.Evaluate` / `Rule.matches` | Matches jurisdiction-scoped rules; jurisdiction ranks above `licensing_mode`, below brand, in `specificity()` (`types.go:253-258`) | **Conditionally fail-closed**: `ErrMissingJurisdiction` only if ≥1 jurisdiction-scoped rule exists for that operation; otherwise proceeds | **Yes** — casino launch/bet, bonus gate |
| K-3 | `casino.ResolveLaunchEligibility` blocklist check | `ErrJurisdictionBlocked` if the game's blocklist contains it | **Skipped entirely — fail-open** | Reachable only in tests |
| K-4 | `casino_launch_sessions` → `postBet` | Carries launch-time jurisdiction across the whole round | Empty → NULL → `RiskRequest.JurisdictionCode = ""` | Yes (always empty) |
| K-5 | `bonus.GateCheckpoint` | Feeds both K-1 and K-2 | Denies at K-1 | **Yes — this is the blocked path** |
| K-6 | `withdrawal.ResolveApprovalPolicy` | `jurisdiction_code IS NULL OR = $5` with jurisdiction-specificity in `ORDER BY` | Every caller passes `nil`; jurisdiction-scoped rows are `CHECK`-forbidden | Query live, dimension disabled |
| K-7 | `rg.ResolveOpenBetSelfExclusionPolicy` | Resolves the jurisdiction floor + tenant/brand tightening | `Configured=false`, caller must fail closed | **No production caller** |
| K-8 | `payments.RouteProvider` | Would drop providers not licensed for the country | Dimension not implemented (`TODO(jurisdiction)`) | **No** |
| K-9 | Bonus `eligibility_jurisdiction_list` / `jurisdiction_restriction` | Would restrict Offer/Campaign eligibility | **Stored, never read** | **No** |
| K-10 | Doc 15's five named enforcement points (registration geo-gating, KYC/AML/RG ruleset resolution, payment method filtering, reporting format selection, provider/catalogue filtering) | Would all read `tenant_jurisdiction_configs` | Table has zero production readers | **No** |

---

## 5. Dependency map — who blocks on whom, and where the gap actually is

```
                        ┌──────────────────────────────────────────┐
                        │  P-1  PLAYER/OPERATION JURISDICTION      │
                        │       RESOLVER  ***DOES NOT EXIST***     │
                        └──────────────────────────────────────────┘
                                          │
      ┌───────────────┬───────────────────┼────────────────┬──────────────────┐
      ▼               ▼                   ▼                ▼                  ▼
 K-1 AssetAuth   K-2 Risk           K-3 casino game   K-6 withdrawal    K-8 payments
 (layer 6)       (jurisdiction-      blocklist        jurisdiction      country
                  scoped rules)                        policy            routing
      │               │                   │                │                  │
      ▼               ▼                   ▼                ▼                  ▼
 K-5 Bonus       casino launch/      fail-OPEN today   DB-disabled      TODO(jurisdiction)
 gate chain      bet risk scope                        (CHECK NULL)
      │
      ├─► Bonus deposit sweep issuance      ─ denied fail-closed (wave-3-report §4)
      ├─► Bonus cashback scheduler issuance ─ denied fail-closed (wave-3-report §4)
      ├─► Bonus expiry/termination          ─ unaffected (value-reducing, ungated)
      └─► DEP-EOI-8 (bonus_campaign_activation EOI with no mint point)
            └─ inert *only because* issuance denies at this gate
                 (docs/architecture/13-dependency-map-and-risk-register.md:85)

  Also gated on the same unresolved dependency (per wave-3-report.md §4/§25):
      • bonus-funded / locked-stake casino wagering (postBet's locked-bonus leg)
      • casino's settlement-timeout architecture
```

**Key finding, stated precisely: the gap is not "jurisdiction is missing".
It is that the platform has a fully-built *jurisdiction configuration and
consumption* layer with a *completely absent production layer* — and the two
halves were built to different fail-behaviour contracts.** Specifically:

- Bonus is blocked because AssetAuthorization chose **unconditional
  fail-closed** (correctly, per ADR 0037 §C.2 and security finding S-6a).
- Casino is *not* blocked, because its two jurisdiction consumers chose
  **fail-open** (K-3) and **conditionally fail-closed** (K-2) respectively.
- Withdrawal and Payments are not blocked because their jurisdiction
  dimensions were deliberately **switched off** (a DB `CHECK`, a `TODO`).

So the same missing producer manifests as a hard block in one domain, a
silent fail-open in another, and a disabled feature in two more. **Closing
the producer gap without first reconciling the four different
absent-value contracts would convert three currently-dormant surfaces into
live ones simultaneously** — including K-3, the per-game jurisdiction
blocklist, which would begin denying launches the moment a real jurisdiction
starts flowing. That is a behavioural-change surface the next phases must
plan for explicitly, not discover.

**Ordering constraint that follows from the map:** no consumer can be
un-blocked before the registry itself is writable. `jurisdictions` has **no
production writer** (P-11), so today a resolver could not return a value that
satisfies any FK. Registry-write surfaces (`jurisdictions`, and whichever of
`licences`/`tenant_jurisdiction_configs` the design ends up needing) are a
hard prerequisite of any resolver, not a follow-up.

---

## 6. Contradictions between existing domain assumptions

Each is a genuine, code-verified inconsistency, not a doc nitpick.

**C-1 — "Jurisdiction" and "licensing mode" are conflated at the only place a
licence is actually consulted.** ADR 0006's model is
*licence → jurisdiction*. But the only licence-derived value any production
code reads is `tenants.licensing_model` — a **binary mode**
(`under_platform_licence` / `own_licence`), not a jurisdiction. `licences.jurisdiction_id`
— the actual link from a tenant to its licensing jurisdiction — is never
read. Risk's `specificity()` (`internal/risk/types.go:253-258`) even ranks
`licensing_mode` *below* `jurisdiction_code` on the explicit reasoning that
"a binary categorization is coarser than an actual jurisdiction" — correct,
and simultaneously an admission that the platform has been using the coarse
proxy because the real value is unreachable.

**C-2 — Jurisdiction configuration has no brand dimension, but three
jurisdiction consumers do.** `tenant_jurisdiction_configs` is keyed
`(tenant_id, jurisdiction_id, effective_from)` — no `brand_id`. Yet
`asset_authorizations` (layers 5 and 6 as independent facts),
`risk_rules` (`brand_id` + `jurisdiction_code`) and
`open_bet_self_exclusion_policies` (`jurisdiction_code` + `tenant_id` +
`brand_id`, tighten-only) all model brand and jurisdiction as **orthogonal**
dimensions. ADR 0012 makes Brand distinct from Tenant, and a tenant may run
multiple brands in different markets. Whether a brand can have its own
operating jurisdiction — concept 5 — is therefore **inconsistently answered
by the schema today**: "no" at the configuration layer, "yes" at three
enforcement layers.

**C-3 — Four different "absent jurisdiction" semantics coexist.**
(a) AssetAuthorization: absent ⇒ **unconditional deny**.
(b) Risk: absent ⇒ **deny only if a jurisdiction-scoped rule exists for that
operation**, else proceed.
(c) Casino game blocklist: absent ⇒ **skip the check** (fail-open).
(d) Payments `SupportedCountries`: empty ⇒ explicitly "not country-restricted"
— i.e. **empty means permissive by contract**
(`internal/payments/types.go:188-191`), the exact inverse of (a).
Any canonical resolver must pick one, and (d) additionally means a PSP
capability's empty set and a resolver's empty answer mean opposite things
while both being empty string slices.

**C-4 — Jurisdiction is currently client-supplied on every staff Bonus
surface, in direct tension with the contract AssetAuthorization states for
its own input.** `CheckEligibility`'s doc comment (`authorization.go:43-45`)
requires "tenant, brand and **jurisdiction** MUST be resolved server-side
from authenticated context by the caller", and `tenant` is additionally
cross-checked against the transaction's `app.tenant_id` GUC so a
request-body tenant cannot be evaluated. **Jurisdiction has no such
backstop**, and Bonus's staff handlers pass a request-body `jurisdiction_code`
straight through (P-3). `casino.LaunchGameParams`' own doc comment names
precisely why this matters: a client-influenced jurisdiction "would let a
player pick a jurisdiction that dodges a jurisdiction-scoped HARD_LIMIT".
The Bonus surfaces are staff-authenticated and permission-gated, so this is
not equivalent to a player-controlled value — but it is a
staff-selectable regulatory scope with no four-eyes control and no
server-side validation that the chosen jurisdiction is one the tenant is
licensed to serve. `security` should be asked to classify it explicitly;
this reconnaissance does not rule on it.

**C-5 — The jurisdiction actually used for a decision is never recorded.**
Two snapshot columns exist for exactly this purpose
(`casino_launch_sessions.jurisdiction_code`, immutability-trigger-protected
by migration 0042; `bonus_grants.jurisdiction_code`, immutable per migration
0057:170) and **neither is ever populated** — Bonus's is not even wired: the
Grant's field and the gate's parameter are two independent values, and only
the gate's is used. A regulator-facing reconstruction of "under which
jurisdiction's rules was this grant issued?" is therefore impossible today
even for the one path where a real code is supplied (P-3).

**C-6 — Casino and Bonus disagree on the temporal grain of an operation's
jurisdiction.** Casino resolves **once per round** and deliberately freezes
it for the session's whole lifetime ("so a jurisdiction-scoped rule stays
reachable from this SAME round's later bets", ADR 0031 §9; enforced by the
immutability trigger). Bonus resolves **per checkpoint** — issue, activate,
convert and held-disposition-resolve each call `resolveJurisdictionID`
independently, so a Grant issued under one jurisdiction could be converted
under another with nothing detecting it. Neither is wrong in isolation;
there is no platform rule reconciling them.

**C-7 — Doc 15 describes an enforcement model that no code implements, and
has already been partially superseded without being updated.** Doc 15
§"Enforcement points" names five consumers of `TenantJurisdictionConfig`;
zero are wired. Migration 0045's own header states
`tenant_jurisdiction_configs.allowed_currencies` is "now redundant",
superseded by `asset_authorizations` — a real architectural change recorded
only in a migration comment and in ADR 0037 §A.5/§C.6.4's open-question list,
never in doc 15 itself. ADR 0037's open question 7 (no product axis on layer
6) is likewise **stale**: migration 0045 shipped a `product` column with
most-specific-match semantics and `CheckEligibility` passes `scope.Product`
through. Both are `architect`-owned doc-corrections; they are named here and
deliberately **not** fixed in this read-only phase.

**C-8 — `kyc_documents.issuing_country` is the only country-shaped player
fact in the system and is none of the three player concepts.** It is the
issuing state of a document. It is not nationality (a passport can be issued
by a state one is not a national of in edge cases, and an ID document's
issuer is not a residence claim), not residence, and not location. It is
named here because it is the artefact a future implementer is most likely to
reach for, and doing so silently would create exactly the conflation the
directive forbids.

---

## 7. Candidate Human Decision Register items — NAMED, NOT DECIDED

Per `CLAUDE.md`'s "When to stop and ask" and the directive's explicit
instruction. **None of these is decided, narrowed, or defaulted by this
document**, and no specialist in the following phases should decide them
either — they are routed to the orchestrator for the human.

- **HDR-J-1 — May a missing player jurisdiction ever fall back to the
  tenant's or brand's jurisdiction?** Named explicitly by the directive as
  something that must not be silently assumed. Concretely load-bearing today:
  it is the difference between the Bonus deposit/cashback sweeps staying
  fail-closed forever and them issuing under an assumed jurisdiction.
  *This is a legal/regulatory question, not an engineering one* — issuing a
  bonus under the wrong jurisdiction's ruleset is a compliance event, not a
  bug. **Not decided here. Not to be defaulted by identity-compliance,
  risk, bonus-engine or architect.**

- **HDR-J-2 — Which player-side signal is legally authoritative for which
  operation class?** The directive requires distinguishing location,
  residence and nationality; it does not (and cannot) say which one governs.
  Real regimes differ: physical presence typically governs *play*, residence
  typically governs *KYC/AML and reporting*, and nationality may govern
  neither or both. Whether these differ per operation (play / deposit /
  withdrawal / bonus issuance / reporting) is jurisdiction-specific legal
  interpretation. Needed before any precedence rule can be written.

- **HDR-J-3 — Is a residence/location/nationality field on a player a
  privacy decision requiring its own lawful basis and retention rule?**
  `docs/architecture/16-privacy.md` deliberately kept PII off
  `player_accounts`; `persons` is platform-scoped and RLS-exempt. Adding a
  player geographic attribute crosses both of those deliberate boundaries.
  Legal/privacy input, not an engineering call.

- **HDR-J-4 — What happens to obligations already in flight when a player's
  jurisdiction changes?** Directly implied by C-6. If a player's resolved
  jurisdiction changes between a Grant's issuance and its conversion, or
  between a launch and a bet, does the original jurisdiction govern, the
  current one, or the stricter of the two? This has consumer-protection
  consequences and is adjacent to (but **must not be answered together
  with**) the already-open `OpenBetSelfExclusionPolicy` item.

- **HDR-J-5 — For a bring-your-own-licence (`own_licence`) tenant, whose
  determination governs?** ADR 0006 supports both models simultaneously. If
  the platform's resolver says jurisdiction X and a BYOL tenant's own licence
  contemplates Y, the platform is not positioned to overrule its tenant's own
  regulator — nor to accept a tenant-supplied value uncritically. Commercial
  + legal. No BYOL tenant exists yet, so this is not urgent, but the answer
  shapes whether the resolver is platform-owned or tenant-overridable, which
  *is* an irreversible-ish design property.

- **HDR-J-6 — Which markets is the first B2C (Anjouan-licensed) brand
  actually permitted to serve?** `licences.permitted_markets` is the column
  that would hold this; it is empty and unread. Any resolver that returns a
  jurisdiction is worthless without the permitted-market list to check it
  against. `CLAUDE.md` already classifies "jurisdiction selection beyond
  Anjouan" as a stop-and-ask item; this is the concrete data that decision
  produces.

**Explicitly untouched by this document**, per the directive: G-2,
`OpenBetSelfExclusionPolicy`'s default value, the mixed/bonus-funded
sportsbook cashout policy, FD-1, and the Grant-cancellation-after-conversion
/ player-receivable question raised in `wave-3-report.md` §21. None was
selected, narrowed, defaulted, or referenced as settled.

---

## 8. Open design questions for the next specialist phase

These are engineering questions for `identity-compliance` to open and the
chain to settle collaboratively. They are deliberately posed as questions,
not recommendations — `architect` has not ruled on any of them.

**Shape of the resolved value**
- **Q-1** — Is the resolver's output a **single code**, or a **resolution
  record** (value + basis + evidence + as-of + confidence)? Every current
  consumer takes a single opaque value (K-1…K-8), and ADR 0031 §9 explicitly
  says "A `RiskRequest` carries exactly one `JurisdictionCode` value, never a
  set — resolving any genuinely ambiguous signal into that one value is the
  caller's job". If the platform must distinguish location/residence/
  nationality (directive), the *resolver* must see several signals and the
  *consumers* still want one answer — so where exactly does the collapse
  happen, and is the pre-collapse evidence persisted? C-5 argues it must be.
- **Q-2** — `uuid` or `code` as the canonical carrier (§1.2)? Today
  `CheckEligibility` needs the id and everything else needs the code, and the
  only translator is package-private inside `internal/bonus`. Should there be
  one canonical type, and where does the translation helper live?
- **Q-3** — What is the canonical **absent-value contract**, given C-3's four
  incompatible ones? Is "absent ⇒ deny" adopted platform-wide (matching
  AssetAuthorization and security finding S-6a), and if so, what is the
  migration plan for K-3 (currently fail-open) and for payments' inverted
  empty-means-permissive semantics?

**Where resolution happens**
- **Q-4** — Is jurisdiction resolved **per operation**, **per session**,
  **snapshotted per entity**, or some combination (C-6)? Casino froze it per
  round with an immutability trigger and a documented rationale; Bonus
  re-resolves per checkpoint. Is one of these the platform rule, or do
  round-shaped and grant-shaped operations legitimately differ?
- **Q-5** — What is the operation jurisdiction for a **system-initiated
  operation with no request context** (the deposit sweep, the cashback
  scheduler, a future expiry/settlement sweep)? This is the precise crux of
  the Bonus blockage (P-4). A sweep has a tenant, a brand, a player and a
  triggering ledger transaction, but no session and no actor context. Whether
  it may derive a jurisdiction from the player, from the triggering
  transaction, or from nothing at all is entangled with **HDR-J-1** and must
  not be answered by assuming the fallback.
- **Q-6** — Which operations are **jurisdiction-bearing at all**? Bonus's
  value-reducing paths (`terminalWriteDown`, `TerminateGrant`) deliberately
  run ungated. Is "value-reducing operations need no jurisdiction" a general
  platform rule, or a Bonus-local one?

**Sources and providers**
- **Q-7** — Is the player-side signal a **provider-abstracted capability**
  (a `GeolocationProvider`-style interface with a mock/sandbox, per
  `CLAUDE.md`'s provider-abstraction rule) or a pure data lookup over
  platform-owned fields? The directive forbids selecting or inventing a real
  vendor; it does not forbid defining the interface such a vendor would later
  implement. Whether defining that interface *now* is in scope, or is
  premature speculative abstraction (`product-owner-proxy`'s remit), is worth
  an explicit ruling.
- **Q-8** — Where does a player residence/nationality attribute live —
  `persons` (platform-scoped, cross-brand, no RLS) or `player_accounts`
  (tenant-scoped, RLS-enforced)? This is a real isolation and privacy
  decision, not a field placement: a cross-brand `persons` attribute is
  visible outside any single tenant's RLS boundary. Gated on **HDR-J-3**.
- **Q-9** — Does `kyc_documents.issuing_country` participate at all, and if
  so under which of the three concepts (C-8)? Answering "none" is a
  legitimate and probably safer answer.

**Registry and configuration prerequisites**
- **Q-10** — `jurisdictions` has no production write surface (P-11). Does
  Stage 4I build one, and does it also need `licences` /
  `tenant_jurisdiction_configs` write surfaces, or is the minimum viable
  slice just the `jurisdictions` registry plus a per-tenant permitted-market
  list? (Gated on **HDR-J-6** for actual content, not for the mechanism.)
- **Q-11** — Does the resolved jurisdiction need to be validated against the
  tenant's **permitted markets** before being usable, and is that the same
  check as, or a separate check from, `tenant_jurisdiction_configs`
  existence? Doc 15 implies a `TenantJurisdictionConfig` row's presence *is*
  the market permission; `licences.permitted_markets` implies a separate,
  licence-level list. Both exist; neither is read.
- **Q-12** — What becomes of `tenant_jurisdiction_configs` given C-7's
  partial supersession? Is it still the KYC/AML/RG/reporting ruleset carrier
  (its remaining four columns), with currencies now owned by
  `asset_authorizations` — and if so, doc 15 needs an `architect` correction.

**Governance and rollout**
- **Q-13** — Is staff-supplied `jurisdiction_code` on Bonus's admin surfaces
  (C-4) acceptable, acceptable-with-validation, or must it be replaced by
  server-side resolution? `security`'s call, not `architect`'s alone.
- **Q-14** — How is the **behavioural-change blast radius** managed (§5)?
  The moment real jurisdiction values flow, K-3 (currently fail-open, so
  currently never denying) begins denying launches, and Risk's conditional
  fail-closed becomes a live path. Does this need a staged rollout, a
  shadow/observe mode, or an explicit tenant-by-tenant enablement flag?
- **Q-15** — Who **owns** the resolver? `docs/governance/ownership.md` has no
  jurisdiction row at all; doc 15 assigns the *schema* to `architect` and the
  *ruleset content* to `identity-compliance`, and names nobody for
  resolution. This needs deciding before implementation, not after.
- **Q-16** — What is the migration path for the deliberately-disabled
  surfaces — `withdrawal_policies`' `CHECK (jurisdiction_code IS NULL)`
  (migration 0033) and payments' routing dimension 2 — and are they in scope
  for Stage 4I or explicitly deferred? Deferring is fine; deferring silently
  is not.
- **Q-17** — Forward-compatibility with retail: doc 26 §1.4 identifies a
  future `hierarchy_nodes.jurisdiction_code` (a registered shop address) as
  "the first authoritative, non-guessed, non-geolocated source of
  jurisdiction this platform would ever have". Retail is explicitly out of
  scope, but the resolver's shape should not make that source awkward to
  plug in later. Worth a one-line check by whoever designs the interface.

---

## 9. Illustrative sketch — NOT DECIDED, NOT A PROPOSAL

Included solely to make Q-1/Q-2/Q-3 concrete for the next phase. **This is
not a design, not a recommendation, and confers no approval.** It is one of
several shapes the chain may consider, and it is written to be easy to
reject.

```go
// ILLUSTRATIVE ONLY — architect has NOT ruled on this shape.
// Shown to make the Q-1 (record vs. single value) and Q-3 (absent-value
// contract) tensions concrete. Do not implement from this.

type JurisdictionBasis string // "player_location" | "player_residence" |
                              // "nationality" | "tenant_licence" |
                              // "brand_operating" | "retail_node" ...

type Resolution struct {
    Code  string    // jurisdictions.code — the ONE value consumers take
    ID    uuid.UUID // jurisdictions.id  — for AssetAuthorization (Q-2)
    Basis JurisdictionBasis // WHY this value — the thing C-5 says is missing
    AsOf  time.Time
    // Signals considered but not selected, for audit reconstruction.
    Considered []struct{ Basis JurisdictionBasis; Code string }
}

// Returns (Resolution{}, ErrUnresolved) rather than a zero value, so no
// caller can mistake "unresolved" for "unscoped" (Q-3 / security S-6a).
func Resolve(ctx, tx, ResolveParams) (Resolution, error)
```

Open even within this sketch, and deliberately unanswered: whether `Basis`
precedence is platform-fixed or per-jurisdiction configuration (**HDR-J-2**);
whether `Considered` is persisted per operation or only audited; whether a
system-initiated sweep may call `Resolve` at all (**Q-5**/**HDR-J-1**).

---

## 10. Headline numbers

- **17** jurisdiction-bearing schema elements inventoried (§1.1). **3** are
  read by production code with a real value (`tenants.licensing_model`,
  `risk_rules.jurisdiction_code`, `asset_authorizations.jurisdiction_id` —
  and the latter two only ever match against configuration, never against a
  resolved player value). **3** have never been read or written by production
  code at all (`licences`, `tenants.licence_id`,
  `tenant_jurisdiction_configs`). **4** are populated-capable but always
  NULL/empty in production (`casino_launch_sessions.jurisdiction_code`,
  `bonus_grants.jurisdiction_code`, `withdrawal_policies.jurisdiction_code`,
  `open_bet_self_exclusion_policies` entirely).
- **11** producers identified (§3); **exactly 1** produces a non-empty
  operational value in production, and it is a staff-supplied HTTP request
  body field.
- **10** consumers identified (§4); **4** absent-value contracts among them,
  mutually inconsistent (C-3).
- **7** concept distinctions required by the directive (§2); **3 do not exist
  in any form** (player location, residence, nationality), **2 exist only as
  unread configuration** (tenant licensing jurisdiction, product
  jurisdiction), **1 does not exist at the configuration layer but is assumed
  by three enforcement layers** (brand operating jurisdiction), and **1
  exists only as two permanently-NULL snapshot columns** (operation
  jurisdiction).
- **8** contradictions recorded (C-1…C-8); **6** candidate Human Decision
  Register items named and not decided; **17** open design questions routed
  to the next phase.

**The single most consequential finding this reconnaissance adds beyond what
`wave-3-report.md` disclosed:** the platform-wide jurisdiction gap is not one
missing resolver feeding a consistent set of consumers. It is a missing
producer feeding **four mutually incompatible absent-value contracts**, three
permanently-unpopulated audit snapshot columns, and an entire configuration
table (`tenant_jurisdiction_configs`) that doc 15 designates as the central
enforcement point for five subsystems and that **no production code has ever
read**. Closing the producer gap is necessary but not sufficient; the
consumer contracts have to be reconciled in the same stage, or the resolver's
first real value will simultaneously unblock Bonus and silently activate a
fail-open control (K-3) that has never once executed.
