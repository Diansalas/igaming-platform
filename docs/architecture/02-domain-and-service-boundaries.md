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
  Every promotional cap, reward-redemption limit, marketplace purchase
  limit, and points earning/spending cap is a rule in `internal/risk`,
  evaluated by `risk.Evaluate` in the same transaction as the effect it
  gates, fail-closed on error — ADR 0031 §13's standing obligation applied
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
