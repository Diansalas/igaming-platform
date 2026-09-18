# 02 — Domain and Service Boundary Proposal

Status: Stage 0 proposal, owned by `architect`. **This document describes
the target shape for later stages, not what exists after Stage 1.** Per
`docs/decisions/0010-stage1-single-service-foundation.md`, Stage 1 ships
exactly one deployable (`platform-api`) organized internally by package
along these same lines; services below are split out into their own
deployables only as real domain logic is built and a concrete ownership/
scaling reason exists, not preemptively.

## Principle

Service boundaries follow the nine core services in Blueprint §4, grouped
by who is authorized to change them (see `.claude/agents/`). A boundary is
correct if a single specialist can own it without needing another
specialist's sign-off for routine changes, while cross-cutting invariants
(money, tenant isolation, audit) are enforced by shared, review-gated
components rather than duplicated per service.

## Proposed services (Stage 1+ scaffolding, not all built at once)

| Service | Owns | Primary specialist |
|---|---|---|
| `identity` | Person/player model, sessions, cross-brand resolution | identity-compliance |
| `tenant-config` | Brand/tenant configuration, versioning | backend / architect |
| `wallet` | Ledger, account balances, idempotent postings | ledger-finance |
| `game-gateway` | Aggregator/provider adapters, catalogue, launch tokens, wallet-callback endpoint | casino |
| `sportsbook-adapter` | Sportsbook provider integration, open-bet liability | sportsbook |
| `bonus-engine` | Campaign/Offer/Grant/Progress, rule evaluation | bonus-engine |
| `gamification` | Points/XP/level rules, achievements, missions, tournaments, leaderboards, streaks — **`NOT IMPLEMENTED`**, architecture frozen Stage 4H-A | gamification (architecture by architect) |
| `retail` / `agent-network` | Configurable hierarchy/agent network, retail terminals & cashier sessions, counter operations — **`NOT IMPLEMENTED`**, architecture frozen Stage 4H-B0 (`26-retail-operations-architecture.md`) | no existing specialist — architecture by architect; implementation owner is an OPEN DECISION for the Orchestrator (see doc 26 §7) |
| `segment` | Segment definitions/versions, static membership, membership resolution — **`NOT IMPLEMENTED`**, architecture frozen Stage 4H-B1 Wave 1.5 (`30-segmentation-engine-architecture.md`) | architect (interface/contract/schema), bonus-engine (first consumer) |
| `crm` | Player lifecycle, customer read-projection, engagement campaigns, audiences, journeys, triggers, communications, preferences, suppression, frequency, experimentation, campaign performance — **`NOT IMPLEMENTED`**, architecture frozen Stage 4H-B1 Wave 1.5 (`31-crm-engine-architecture.md`) | no existing specialist — architecture by architect; implementation owner is an OPEN DECISION for the Orchestrator |
| `affiliate` | Affiliate/partner records, tracking, clicks, attribution evidence and decisions, promo codes as attribution tokens, commission rules/accrual/approval, affiliate reporting definitions — **`NOT IMPLEMENTED`**, architecture frozen Stage 4H-B1 Wave 1.5 (`32-affiliate-and-acquisition-architecture.md`) | no existing specialist — architecture by architect; implementation owner is an OPEN DECISION for the Orchestrator |
| `payment-orchestrator` | PSP/crypto routing, reserve accounting, withdrawal workflow | payments |
| `compliance` | KYC/AML orchestration, RG controls, case queue | identity-compliance |
| `backoffice-api` / `partner-console-api` | Admin operations, RBAC-gated | backend / backoffice |
| `audit` | Append-only audit log, shared library used by every mutating service | security (design), backend (implementation) |
| `event-bus` (Kafka/NATS) | Cross-service event distribution | integrations / architect |
| `reporting` | CDC → ClickHouse, report definitions | data-analytics |

`RECOMMENDATION`: start Stage 1 with fewer, coarser services (e.g.
`wallet`, `game-gateway`, `identity+compliance`, `backoffice-api` as one
deployable each) and split further only when a real scaling or
ownership need appears — per Blueprint's own bias toward not
over-building ahead of proven need (§13).

## Contract discipline

- Every cross-service call is a versioned, documented API (internal or
  external) — never a shared database table written by two services.
- Only `wallet` may write ledger tables. Every other service that needs to
  move money calls `wallet`'s API.
- Only `audit`'s shared library appends to the audit store; services call
  it, they don't implement their own audit writer.
- `tenant-config` is the single source of truth for what a brand looks
  like; no service caches tenant config longer than its documented TTL.
- Only the Reward Orchestrator turns a reward *decision* into a fulfilled
  reward. Domains that decide a reward is owed (`bonus-engine`,
  `gamification`, tournaments, missions, the future reward marketplace)
  emit a decision and stop; they never credit a wallet, a points balance,
  or an externally-fulfilled reward themselves (Stage 4H-A architecture
  freeze — `NOT IMPLEMENTED`).

## What is explicitly NOT a separate service (yet)

Per `CLAUDE.md`'s scope-expansion test, the following stay inside an
existing service until a real need forces a split: affiliate tracking
in-house rebuild (Blueprint §1 table — third-party first, in-house is a
year-two consideration), CRM/campaign journey builder (buy first), and any
per-jurisdiction reporting engine beyond a pluggable export interface.

**Stage 4H-B1 Wave 1.5 clarification — not a reversal.** A human
directive elevated CRM and Affiliate to first-class *architectural*
domains (`31-crm-engine-architecture.md`, `32-affiliate-and-acquisition-
architecture.md`). That does not overturn the buy-first posture above,
and neither document claims it does: what those documents freeze is the
**boundary, data ownership, enforcement points and financial contract** —
i.e. exactly what a bought product must plug into rather than replace,
per `CLAUDE.md`'s provider-abstraction rule. Buy-vs-build for the CRM
journey execution engine (doc 31 OI-CRM-2) and for the affiliate platform
(doc 32 OI-AFF-1) both remain OPEN DECISIONS for the human. Neither is a
separate deployable — ADR 0010 is untouched; both are packages.

## Open question

Whether `identity` and `compliance` should be one service or two is
deferred to Stage 2 design, when the actual KYC vendor contract shape
(Blueprint §10 Q2 dependency) is known.

## Cross-domain boundary verification (Stage 4G-FINAL)

Still one deployable internally organized by package (ADR 0010 - this
document remains a target proposal, not yet the built shape). Stage
4G-FINAL's own directive required verifying that Casino, Payments,
Financial/Ledger, RG, Identity, KYC, and Risk remain separate domains
with explicit contracts, none silently duplicating another's authority.
Verified as of this stage:

| Domain | Package | Sole authority for | Never does |
|---|---|---|---|
| Financial/Ledger | `internal/ledger`, `internal/wallet` | Posting money, balance projections, idempotency | Business/eligibility decisions about WHETHER to post |
| Identity | `internal/identity` | Person/PlayerAccount/StaffUser/Tenant/Brand records | Auth session mechanics (that's `internal/auth`), risk/RG decisions |
| KYC | `internal/kyc` | Verification/document lifecycle and evidence | Blocking play/withdrawal directly (RG/withdrawal policy enforce, KYC only reports verification state) |
| Responsible Gaming | `internal/rg` | Self-exclusion/RG eligibility (`EvaluateEligibility`) | Generic risk/limit policy - has no rule/threshold concept beyond RG's own restrictions |
| Risk Management | `internal/risk` | Configurable risk/limit policy (`Evaluate`) | Self-exclusion - has no player-status concept beyond what a caller passes in as request fields |
| Casino | `internal/casino` | Game gateway, launch, bet/win/rollback callbacks | Ledger schema, RG/Risk rule logic - calls their interfaces, never reimplements them |
| Payments | `internal/payments`, `internal/withdrawal` | PSP orchestration, deposit/withdrawal state machine | Ledger schema (calls `ledger.Post` like every other domain) |

Confirmed by inspection (not merely by convention): `internal/risk` has
zero imports of `internal/rg` and vice versa. `internal/httpserver`
imports both (`rg_handlers.go`, `risk_handlers.go` - separate files, each
its own domain's admin API), but `internal/casino/orchestrator.go` is the
only file that COMPOSES both decisions on a single operation's path
(`rg.EvaluateEligibility` then `risk.Evaluate`, in that fixed order, RG
always short-circuiting Risk on denial - ADR 0031 §1; corrected from an
earlier, imprecise "only caller that imports both" wording per
documentation review).

`internal/casino`'s one new Stage-4G-FINAL dependency,
`internal/identity.GetTenantByID` (resolving `LicensingMode` for a
`RiskRequest`), is a plain read of a platform-registry table Identity
already owns (`tenants`, no RLS) - it does not cross a domain's WRITE
authority, and was filed through `integration-protocol.md`'s
dependency-request procedure (see `task-registry.md`'s Dependency Request
Log, `DR-4GF-03` - corrected from an earlier version of this paragraph
that miscited `DR-4GF-01`/`DR-4GF-02`, which cover the risk/casino
dependencies, not this identity one).

No ambiguous boundary was found this stage. The one PRE-EXISTING
observation worth naming explicitly (not a defect, already noted in ADR
0031 §8): `internal/risk`'s generic `Rule` shape COULD, in principle, be
misused to express an RG-shaped "block this player entirely" rule (e.g.
a player-scoped `HARD_LIMIT` with a zero `min_amount` threshold on every
operation) - RBAC already prevents this in practice (`risk_config:manage`
and RG's own staff permissions are held by different roles), but nothing
in `internal/risk` itself rejects such a rule at the type level. Recorded
here as a documentation note, consistent with ADR 0031's own treatment -
not fixed, since it would require either a policy-level convention (staff
guidance: "risk rules govern limits, not identity-based prohibition") or
a schema-level restriction neither this stage's directive nor any
specialist review this stage flagged as urgent.

## Gamification Engine boundary (Stage 4H-A — architecture freeze)

Status: **`NOT IMPLEMENTED`.** No `internal/gamification` package, no
schema, no migration, no API exists. Stage 4H-A froze the architecture only
(`17-gamification-engine-architecture.md`, `18-tournament-architecture.md`,
`19-mission-architecture.md`, `20-reward-marketplace-architecture.md`).
This section states the boundary the eventual implementation must satisfy,
in the same Owns / Never does style as the verified table above — it
records a *contract*, not a verification, because there is nothing built to
verify yet.

| Domain | Package (future) | Sole authority for | Never does |
|---|---|---|---|
| Gamification | `internal/gamification` | Points earning/spending **rules** (when and how much), XP accrual, level definitions and current level, achievement/badge definitions and unlocks, mission definitions and progression, tournament definitions/entry/scoring/settlement decisions, leaderboard ranking and immutable result snapshots, streak state | Post to the ledger; credit a points balance; grant a bonus; fulfil any reward; own the points ledger schema; define limits/caps; define self-exclusion; accept a provider-specific payload |

The five boundaries that matter most, each mirroring a separation this
codebase has already established rather than inventing a new one:

- **Gamification is NOT a Risk replacement.** It has **no limit engine, no
  threshold, no cap, no counter, and no velocity concept of its own**.
  Every promotional cap, reward-redemption limit, and marketplace purchase
  limit is a rule in `internal/risk`, evaluated by `risk.Evaluate` in the
  same transaction as the effect it gates, fail-closed on error — ADR 0031
  §13's standing obligation applied. **Specialist-review correction**: an
  earlier draft of this bullet also listed "points earning/spending cap"
  as already covered by this rule — it is not.
  `docs/decisions/0031-risk-and-limits-engine.md` §15h explicitly puts
  points earning/spending OUT of Risk's scope while points remain
  non-convertible/non-withdrawable/non-transferable. Gamification's
  refusal to build its own limit engine still holds (it may not invent a
  points cap either), but the honest state today is that **no points
  earning/spending cap mechanism exists anywhere** — see
  `docs/architecture/17-gamification-engine-architecture.md` §3.3 for the
  full disclosure and the two possible future resolutions, neither
  authorized this stage
  to this domain. Several of these want limit kinds ADR 0031 §4
  deliberately did not implement (`count`, `velocity`); the answer is ADR
  0031 §12's five-step extension process, never a counter inside
  Gamification.
- **Gamification is NOT an RG replacement.** It has **no self-exclusion
  concept**, never reads or writes `player_restrictions`, and never caches
  an eligibility answer. It calls `rg.EvaluateEligibility` before any
  player-facing gamification action — earning or spending points, mission
  opt-in/progress/completion, tournament entry or scoring, achievement
  unlock, streak advance, marketplace redemption, or any reward triggered
  by these — exactly as `internal/casino` already does. Order is fixed: RG
  first, then Risk, with an RG denial short-circuiting before Risk is
  evaluated (ADR 0031 §1).
- **Gamification never creates financial liability.** It emits a reward
  *decision*; the Reward Orchestrator owns fulfilment, retry, failure, and
  reversal, and routes to whichever domain owns the mechanism. No
  gamification package may import `internal/ledger`, `internal/wallet`, or
  the Bonus Engine.
- **Gamification does not own the points ledger.** It decides *when* and
  *how much*; `ledger-finance` owns the points accounting model
  (`24-points-accounting-architecture.md`, not yet written). Same split as
  `internal/casino` deciding a bet and `internal/ledger` posting it — no
  second balance system, for money or for points.
- **Gamification consumes canonical events only.** It never accepts a
  casino-, sportsbook-, or payments-provider-specific payload, and no
  provider identifier or provider game id appears in a gamification rule.
  Adding a provider must require zero gamification changes — verifiable by
  import inspection the same way the Risk/RG separation already is.

Gamification is also a sibling of the Bonus Engine, not a layer of it: the
Bonus Engine owns monetary promotional liability (Campaign/Offer/Grant/
Progress, wagering, forfeiture); Gamification owns behavioural progression
state. A gamification reward that happens to be bonus-shaped is fulfilled
by the Bonus Engine *via* the Reward Orchestrator, never by Gamification
calling the Bonus Engine directly.

## Retail / Agent Network boundary (Stage 4H-B0 — architecture freeze)

Status: **`NOT IMPLEMENTED`.** No `internal/retail` or
`internal/agentnetwork` package, no schema, no migration, no API, no test
exists. Stage 4H-B0 froze the architecture only — see
`docs/architecture/26-retail-operations-architecture.md`, which is
authoritative for this domain and which this section summarizes rather
than restates. Like the Gamification section above, this records a
*contract* the eventual implementation must satisfy, not a verification,
because there is nothing built to verify.

**Scope anchor, stated honestly** (`CLAUDE.md`'s "never claim a Blueprint
requirement that isn't there"): `iGaming-Platform-Blueprint.pdf` contains
**no** retail, land-based, agent-network, POS, terminal, shop or kiosk
requirement — verified by full-text search of all 20 pages (the three
occurrences of "cashier" all mean the *online* player-facing deposit/
withdraw screen). Retail enters the project as a human-directed business
requirement via the Orchestrator (Stage 4H-B0), which is a legitimate
authority for scope but is **not** the Blueprint and must never be cited
as such. This is structurally the same unanchored-scope situation
`product-owner-proxy` recorded for Gamification in Stage 4H-A; doc 26 §0.1
recommends the Orchestrator record it in `14-mvp-scope-and-roadmap.md` the
same way.

| Domain | Package (future) | Sole authority for | Never does |
|---|---|---|---|
| Retail / Agent Network | `internal/agentnetwork` (hierarchy primitive) + `internal/retail` (operational surface) — split recommended, doc 26 §7.1 | The configurable hierarchy: node **types**, allowed parent→child **structure**, node **capabilities**, node instances and their parent edges, the derived closure projection, node↔staff assignments, terminal registration and cashier shift sessions, counter-operation orchestration and its idempotency records, retail player-origin attribution, and the **structural** authorization question "may node A move value to node B, and is this actor authorized over node A" | Post to the ledger; own any balance, float, commission accrual or settlement amount; define a limit, cap, counter or velocity; define eligibility or self-exclusion; define a KYC tier; write its own audit records; accept a client-supplied tenant id, node id or parent claim; create a second identity, wallet, or player model |

The boundaries that matter most, each mirroring a separation this
codebase already established rather than inventing a new one:

- **Retail is not a second financial system.** A terminal is an API
  client of the same platform, with no terminal-local balance, no
  terminal-side authorization decision, and no offline acceptance of any
  balance-affecting operation (doc 26 §2.5 — an offline requirement is an
  `OPEN DECISION` for the human, not an assumed capability). `CLAUDE.md`'s
  "the authoritative balance read happens inside the same database
  transaction as the write" applies to a counter terminal exactly as it
  applies to a browser.
- **Retail owns no money.** It emits the *reason* for a movement (float
  advance, settlement, commission, counter deposit/payout) and the
  *authorization* for it; `ledger-finance` owns every account, posting and
  monetary invariant — `docs/decisions/0035-*`. This is ADR 0032's
  precedent applied unchanged: if doc 26 and ADR 0035 ever disagree on
  monetary treatment, **ADR 0035 wins.** No retail package may import
  `internal/ledger` or `internal/wallet` to decide anything monetary
  itself.
- **Retail is NOT a Risk replacement.** It has no limit engine, no
  threshold, no cap, no counter and no velocity concept of its own. Float
  ceilings, per-cashier payout limits, daily counter-deposit caps and
  cash-structuring thresholds are rules in `internal/risk`, evaluated by
  `risk.Evaluate` in the same transaction as the effect they gate,
  fail-closed on error (ADR 0031 §13). New `Operation` values and any new
  `LimitKind` go through ADR 0031 §12's extension process, owned by
  `risk` — never a counter inside retail.
- **Retail is NOT an RG or KYC replacement.** Every player-facing counter
  operation calls `rg.EvaluateEligibility` first, then `risk.Evaluate`,
  in that fixed order with RG short-circuiting on denial (ADR 0031 §1) —
  this is ADR 0027's own standing forward note ("any FUTURE endpoint that
  can move money or let a player gamble MUST call `EvaluateEligibility`")
  applied to exactly the endpoints it was written for. KYC tiers, AML
  treatment of cash, and physical-presence self-exclusion are
  `identity-compliance`'s, flagged in doc 26 §3.3 and not invented there.
- **Retail creates no second identity model.** A player registered at a
  counter is an ordinary `Person` + `PlayerAccount` under a
  `(tenant_id, brand_id)` pair, created through the unchanged
  `identityresolution.RegisterPlayerWithResolution` flow (ADR 0027). A
  cashier is an `identity.StaffUser` with a retail role — **not** a new
  actor or principal type, so `audit.ActorType` and `auth.PrincipalType`
  are unchanged. A terminal is a `service` principal, which makes ADR
  0014's option 2 real for the first time. **Neither a player nor a
  cashier is a node in the hierarchy** (doc 26 §1.6) — they attach to a
  node by reference.
- **The hierarchy is configuration, never a code path.** No table,
  constraint, enum, Go type or permission may contain
  `operator`/`partner`/`super_agent`/`agent`/`cashier` as a structural
  element; that chain is seed data in a configuration table, and a
  different tenant's network is a different set of rows with zero code
  change. This is `CLAUDE.md`'s "nothing brand-specific may become a code
  path" applied to network topology. An integer `level` column is
  explicitly rejected. Node **type/relation/capability definitions** are
  dual-scope (`tenant_id IS NULL` = platform-wide template), mirroring
  `risk_rules` (ADR 0031 §3) and the `PointType` correction (doc 24 §2);
  every node **instance** row is `tenant_id NOT NULL`.

### Open cross-domain items this boundary depends on

Doc 26 §8 flags these explicitly rather than resolving them; **none is a
silent change to previously approved architecture**, and each needs its
named owner's sign-off before any retail implementation:

| Item | Owner(s) | Severity |
|---|---|---|
| Node float vs. ADR 0007's explicitly player-centric `Wallet` model (amending ADR 0007 would need human sign-off — it is a recorded human decision) | **ledger-finance** (ADR 0035) | **P0** |
| A node-subtree RLS dimension (`app.hierarchy_node_id` + `WithNodeScope`), including the permissive-policy OR-widening split ADR 0019 already documents for `app.player_account_id` | architect + security | P1 |
| Whether `player_accounts`' RLS gains a node dimension (doc 26 recommends **no**, and routing subtree reads through a retail-owned attribution table instead) | identity-compliance + security | P1 |
| New `StaffRole` values and retail permissions (`staff_users`' table shape itself is unchanged) | identity-compliance + security | P2 |
| `risk.Operation` extension for retail operations | risk | P2 |
| A node/terminal dimension on audit records | architect + security | P2 |

Two findings are explicitly **not** conflicts: ADR 0010's
single-deployable shape is untouched (retail adds packages, not
deployables), and jurisdiction needs no change to doc 15/ADR 0006/ADR 0012
— a `hierarchy_nodes.jurisdiction_code` resolves against the existing
tenant-level `TenantJurisdictionConfig`. That second one is a **positive
finding**: a registered shop address would be the first authoritative,
non-geolocated source of jurisdiction this platform has ever had, against
the `TODO(jurisdiction)` gap doc 15 and ADR 0031 §9 both still record.

## Segmentation / CRM / Affiliate boundaries (Stage 4H-B1 Wave 1.5 — architecture freeze)

Status: **`NOT IMPLEMENTED`.** No `internal/segment`, `internal/crm` or
`internal/affiliate` package, schema, migration, API or test exists.
Stage 4H-B1 Wave 1.5 froze the architecture only —
`30-segmentation-engine-architecture.md`,
`31-crm-engine-architecture.md`,
`32-affiliate-and-acquisition-architecture.md`, with the cross-domain
flow diagrams, the per-domain non-duplication register and the
build-order dependency graph in
`33-cross-domain-commercial-flow-map.md`. Those documents are
authoritative; this section records the *contract* in the same Owns /
Never does style as the verified table above, and does not restate them.

| Domain | Package (future) | Sole authority for | Never does |
|---|---|---|---|
| Segmentation | `internal/segment` | Segment definitions, immutable criteria versions, static membership (append-only events), and the deterministic membership *resolution* that answers "is this player in this audience, as of this instant" | Own, compute, cache or override any authoritative fact — no risk score/tier, no RG concept, no KYC model, no jurisdiction resolver, no LTV ledger, no FX conversion, no cross-player ranking, no materialized dynamic membership, no player-facing surface, no gate |
| CRM | `internal/crm` | WHO/WHEN/WHAT CAMPAIGN: player lifecycle classification, the customer read-projection, engagement campaigns/journeys/triggers, communication orchestration and channel abstraction, channel preferences, suppression, message-frequency caps, experimentation, campaign performance definitions | Own a wallet balance, the ledger, bonus accounting, a Risk decision, an RG decision, a KYC decision, identity truth, a consent store, a segmentation engine, a reward fulfilment path, or a BI pipeline. Never supplies a bonus amount or term — it references an `offer_id` + `offer_version_id` and Bonus decides |
| Affiliate / Acquisition | `internal/affiliate` (operational surface) + node instances in `internal/agentnetwork` (hierarchy) | WHO INTRODUCED WHOM and WHAT COMMISSION IS OWED under which versioned rule: tracking links/codes, immutable click and candidate evidence, the frozen attribution decision, commission rules/accrual/adjustment/approval, affiliate reporting definitions | Post to the ledger, touch any player balance, grant a bonus, gate play, define the revenue (NGR/GGR) measure, design the commission posting shape, build a second hierarchy/closure table, create a second identity model, hold a player-risk heuristic, or expose player PII to an external affiliate |

The boundaries that matter most, each mirroring a separation this
codebase has already established rather than inventing a new one:

- **None of the three is a Risk replacement.** This is now the third,
  fourth and fifth occurrence of the same rule (Gamification and Retail
  above were the first two). Segmentation reads `internal/risk` and never
  computes a classification — and doc 30 §8.3 records the honest finding
  that **no persistent player risk classification exists today**, so that
  criterion is `BLOCKED` rather than approximated. CRM caps how often we
  *talk* to a player; only Risk caps how much *value* moves. Affiliate
  has no limit, counter or velocity concept, and self-referral fraud is
  `risk` + `bonus-engine` + `identity-compliance`'s, not an
  affiliate-local heuristic.
- **None of the three is an RG replacement.** Enforcement is always a
  literal `rg.EvaluateEligibility` call by the acting domain, inside the
  acting transaction (ADR 0034 §2.1; doc 21's corrected *unconditional*
  re-check rule). `rg.status.changed` triggers a re-evaluation; it is
  never the answer. A segment may **suppress** an audience on an RG
  signal but may never **constitute** one (doc 30 §8.4 — an engineering
  default with a compliance dimension, routed to the human, tightenable
  but not loosenable).
- **A segment result is evidence, never authorization.** Membership may
  make a player eligible for an Offer's *terms*; it can never make a
  player authorized to *receive value*. The gate chain
  `AssetAuthorization → RG → Risk` is unchanged, unreordered, uncached
  and unbypassable, and no segment/CRM/affiliate symbol may appear
  between a gate call and its enforcement branch.
- **CRM creates no player value and Affiliate moves no money.** CRM is
  not a reward-deciding domain in doc 21's sense and has no interface to
  the Reward Orchestrator at all (a deliberate omission, doc 31 §7.3).
  Affiliate hands `ledger-finance` a `CommissionSettlementInstruction`
  and stops — the identical split ADR 0035 made for retail, where
  `ledger-finance` owned `agent_float` accounting while the operational
  architecture was `architect`'s, and where ADR 0035 wins any monetary
  disagreement.
- **No second identity, hierarchy or consent model.** Affiliate users are
  `identity.StaffUser`s with affiliate roles scoped to a node subtree
  (pending `security`'s judgment on an *external* counterparty in the
  staff principal space, doc 32 DEP-AFF-1); the affiliate tree is
  `internal/agentnetwork`'s — the second consumer doc 26 §7.1 explicitly
  predicted when it refused to name that package `retail`. Marketing
  consent is `identity-compliance`'s and **does not exist yet** (doc 31
  §8.1, DEP-CRM-1); CRM fails closed until it does.

### Open cross-domain items these boundaries depend on

| Item | Owner(s) | Severity |
|---|---|---|
| A marketing/communication consent model — none exists anywhere in the platform today | **identity-compliance** (DEP-CRM-1) | **P0 for any CRM send** |
| Commission settlement posting shape, the canonical NGR/GGR definition, liability recognition, clawback treatment | **ledger-finance** (DEP-AFF-4) + human on commercial policy | **P0 for any settlement** |
| Event transport (transactional outbox vs. broker) — CRM's journey/trigger engine is the first domain that genuinely requires one | Orchestrator → human (doc 22 open decision 1) | P1 |
| Node-subtree RLS dimension (`app.hierarchy_node_id` + `WithNodeScope`) — already flagged P1 by doc 26 §8; Affiliate is its second consumer | architect + security | P1 |
| Affiliate users as subtree-scoped `StaffUser`s vs. a distinct principal type | security (DEP-AFF-1) | P1 |
| The Eligibility Decision Record's versioned-provenance content (doc 30 §7) extends doc 10 W2.4's activation projection | bonus-engine + ledger-finance (DEP-SEG-1) | P1 |
| CRM/affiliate reporting dimensions in the doc 12 pipeline | data-analytics (DEP-CRM-2) | P2 |
| Promo-code vs. bonus-coupon namespace collision check | bonus-engine (DEP-AFF-3) | P2 |
